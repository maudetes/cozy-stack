package vfsafero

// #nosec
import (
	"bytes"
	"crypto/md5"
	"fmt"
	"hash"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/pkg/vfs"
	"github.com/spf13/afero"
)

// aferoVFS is a struct implementing the vfs.VFS interface associated with
// an afero.Fs filesystem. The indexing of the elements of the filesystem is
// done in couchdb.
type aferoVFS struct {
	vfs.Indexer

	fs  afero.Fs
	pth string

	// whether or not the localfilesystem requires an initialisation of its root
	// directory
	osFS bool
}

// New returns a vfs.VFS instance associated with the specified indexer and
// storage url.
//
// The supported scheme of the storage url are file://, for an OS-FS store, and
// mem:// for an in-memory store. The backend used is the afero package.
func New(index vfs.Indexer, fsURL *url.URL, domain string) (vfs.VFS, error) {
	if fsURL.Scheme != "mem" && fsURL.Path == "" {
		return nil, fmt.Errorf("vfsafero: please check the supplied fs url: %s",
			fsURL.String())
	}
	if domain == "" {
		return nil, fmt.Errorf("vfsafero: specified domain is empty")
	}
	pth := path.Join(fsURL.Path, domain)
	var fs afero.Fs
	switch fsURL.Scheme {
	case "file":
		fs = afero.NewBasePathFs(afero.NewOsFs(), pth)
	case "mem":
		fs = afero.NewMemMapFs()
	default:
		return nil, fmt.Errorf("vfsafero: non supported scheme %s", fsURL.Scheme)
	}
	return &aferoVFS{
		Indexer: index,

		fs:  fs,
		pth: pth,
		// for now, only the file:// scheme needs a specific initialisation of its
		// root directory.
		osFS: fsURL.Scheme == "file",
	}, nil
}

// Init creates the root directory document and the trash directory for this
// file system.
func (afs *aferoVFS) InitFs() error {
	if err := afs.Indexer.InitIndex(); err != nil {
		return err
	}
	// for a file:// fs, we need to create the root directory container
	if afs.osFS {
		if err := afero.NewOsFs().MkdirAll(afs.pth, 0755); err != nil {
			return err
		}
	}
	if err := afs.fs.Mkdir(vfs.TrashDirName, 0755); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

// Delete removes all the elements associated with the filesystem.
func (afs *aferoVFS) Delete() error {
	if afs.osFS {
		return afero.NewOsFs().RemoveAll(afs.pth)
	}
	return nil
}

func (afs *aferoVFS) CreateDir(doc *vfs.DirDoc) error {
	pth, err := doc.Path(afs)
	if err != nil {
		return err
	}
	err = afs.fs.Mkdir(pth, 0755)
	if err != nil {
		return err
	}
	err = afs.Indexer.CreateDirDoc(doc)
	if err != nil {
		afs.fs.Remove(pth) // #nosec
	}
	return err
}

func (afs *aferoVFS) CreateFile(newdoc, olddoc *vfs.FileDoc) (vfs.File, error) {
	newpath, err := newdoc.Path(afs)
	if err != nil {
		return nil, err
	}

	var bakpath string
	if olddoc != nil {
		bakpath = fmt.Sprintf("/.%s_%s", olddoc.ID(), olddoc.Rev())
		if err = safeRenameFile(afs.fs, newpath, bakpath); err != nil {
			// in case of a concurrent access to this method, it can happened
			// that the file has already been renamed. In this case the
			// safeRenameFile will return an os.ErrNotExist error. But this
			// error is misleading since it does not reflect the conflict.
			if os.IsNotExist(err) {
				err = vfs.ErrConflict
			}
			return nil, err
		}
	}

	if olddoc != nil {
		newdoc.SetID(olddoc.ID())
		newdoc.SetRev(olddoc.Rev())
		newdoc.CreatedAt = olddoc.CreatedAt
	}

	f, err := safeCreateFile(newpath, newdoc.Mode(), afs.fs)
	if err != nil {
		return nil, err
	}

	hash := md5.New() // #nosec
	extractor := vfs.NewMetaExtractor(newdoc)

	return &aferoFileCreation{
		w: 0,
		f: f,

		afs:     afs,
		newdoc:  newdoc,
		olddoc:  olddoc,
		bakpath: bakpath,
		newpath: newpath,

		hash: hash,
		meta: extractor,
	}, nil
}

// UpdateFileDoc overrides the indexer's one since the afero.Fs is by essence
// also indexed by path. When moving a file, the index has to be moved and the
// filesystem should also be updated.
//
// @override Indexer.UpdateFileDoc
func (afs *aferoVFS) UpdateFileDoc(olddoc, newdoc *vfs.FileDoc) error {
	if newdoc.DirID != olddoc.DirID || newdoc.DocName != olddoc.DocName {
		oldpath, err := olddoc.Path(afs)
		if err != nil {
			return err
		}
		newpath, err := newdoc.Path(afs)
		if err != nil {
			return err
		}
		err = safeRenameFile(afs.fs, oldpath, newpath)
		if err != nil {
			return err
		}
	}
	if newdoc.Executable != olddoc.Executable {
		newpath, err := newdoc.Path(afs)
		if err != nil {
			return err
		}
		err = afs.fs.Chmod(newpath, newdoc.Mode())
		if err != nil {
			return err
		}
	}
	return afs.Indexer.UpdateFileDoc(olddoc, newdoc)
}

// UpdateDirDoc overrides the indexer's one since the afero.Fs is by essence
// also indexed by path. When moving a file, the index has to be moved and the
// filesystem should also be updated.
//
// @override Indexer.UpdateDirDoc
func (afs *aferoVFS) UpdateDirDoc(olddoc, newdoc *vfs.DirDoc) error {
	if newdoc.Fullpath != olddoc.Fullpath {
		if err := safeRenameDir(afs, olddoc.Fullpath, newdoc.Fullpath); err != nil {
			return err
		}
	}
	return afs.Indexer.UpdateDirDoc(olddoc, newdoc)
}

func (afs *aferoVFS) DestroyDirContent(doc *vfs.DirDoc) error {
	iter := afs.Indexer.DirIterator(doc, nil)
	for {
		d, f, err := iter.Next()
		if err == vfs.ErrIteratorDone {
			break
		}
		if d != nil {
			err = afs.DestroyDirAndContent(d)
		} else {
			err = afs.DestroyFile(f)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (afs *aferoVFS) DestroyDirAndContent(doc *vfs.DirDoc) error {
	err := afs.DestroyDirContent(doc)
	if err != nil {
		return err
	}
	dirpath, err := doc.Path(afs)
	if err != nil {
		return err
	}
	err = afs.fs.RemoveAll(dirpath)
	if err != nil {
		return err
	}
	return afs.Indexer.DeleteDirDoc(doc)
}

func (afs *aferoVFS) DestroyFile(doc *vfs.FileDoc) error {
	path, err := doc.Path(afs)
	if err != nil {
		return err
	}
	err = afs.fs.Remove(path)
	if err != nil {
		return err
	}
	return afs.Indexer.DeleteFileDoc(doc)
}

func (afs *aferoVFS) OpenFile(doc *vfs.FileDoc) (vfs.File, error) {
	name, err := doc.Path(afs)
	if err != nil {
		return nil, err
	}
	f, err := afs.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return &aferoFileOpen{f}, nil
}

// aferoFileOpen represents a file handle opened for reading.
type aferoFileOpen struct {
	f afero.File
}

func (f *aferoFileOpen) Read(p []byte) (int, error) {
	return f.f.Read(p)
}

func (f *aferoFileOpen) Seek(offset int64, whence int) (int64, error) {
	return f.f.Seek(offset, whence)
}

func (f *aferoFileOpen) Write(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (f *aferoFileOpen) Close() error {
	return f.f.Close()
}

// aferoFileCreation represents a file open for writing. It is used to
// create of file or to modify the content of a file.
//
// aferoFileCreation implements io.WriteCloser.
type aferoFileCreation struct {
	f       afero.File         // file handle
	w       int64              // total size written
	afs     *aferoVFS          // parent vfs
	newdoc  *vfs.FileDoc       // new document
	olddoc  *vfs.FileDoc       // old document if any
	newpath string             // file new path
	bakpath string             // backup file path in case of modifying an existing file
	hash    hash.Hash          // hash we build up along the file
	meta    *vfs.MetaExtractor // extracts metadata from the content
	err     error              // write error
}

func (f *aferoFileCreation) Read(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (f *aferoFileCreation) Seek(offset int64, whence int) (int64, error) {
	return 0, os.ErrInvalid
}

func (f *aferoFileCreation) Write(p []byte) (int, error) {
	n, err := f.f.Write(p)
	if err != nil {
		f.err = err
		return n, err
	}

	f.w += int64(n)

	if f.meta != nil {
		if _, err = (*f.meta).Write(p); err != nil {
			(*f.meta).Abort(err)
			f.meta = nil
		}
	}

	_, err = f.hash.Write(p)
	return n, err
}

func (f *aferoFileCreation) Close() (err error) {
	defer func() {
		if err == nil && f.olddoc != nil {
			// remove the backup if no error occured
			f.afs.fs.Remove(f.bakpath) // #nosec
		} else if err != nil && f.olddoc != nil {
			// put back backup file revision in case on error occurred
			f.afs.fs.Rename(f.bakpath, f.newpath) // #nosec
		} else if err != nil {
			// remove the new file if an error occured
			f.afs.fs.Remove(f.newpath) // #nosec
		}
	}()

	if err = f.f.Close(); err != nil {
		if f.meta != nil {
			(*f.meta).Abort(err)
		}
		return err
	}

	if f.err != nil {
		return f.err
	}

	newdoc, olddoc, written := f.newdoc, f.olddoc, f.w

	if f.meta != nil {
		if errc := (*f.meta).Close(); errc == nil {
			newdoc.Metadata = (*f.meta).Result()
		}
	}

	md5sum := f.hash.Sum(nil)
	if newdoc.MD5Sum == nil {
		newdoc.MD5Sum = md5sum
	}

	if !bytes.Equal(newdoc.MD5Sum, md5sum) {
		err = vfs.ErrInvalidHash
		return err
	}

	if newdoc.ByteSize < 0 {
		newdoc.ByteSize = written
	}

	if newdoc.ByteSize != written {
		err = vfs.ErrContentLengthMismatch
		return err
	}

	if olddoc != nil {
		err = f.afs.Indexer.UpdateFileDoc(olddoc, newdoc)
	} else {
		err = f.afs.Indexer.CreateFileDoc(newdoc)
	}
	return err
}

func safeCreateFile(name string, mode os.FileMode, fs afero.Fs) (afero.File, error) {
	// write only (O_WRONLY), try to create the file and check that it
	// does not already exist (O_CREATE|O_EXCL).
	flag := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	return fs.OpenFile(name, flag, mode)
}

func safeRenameFile(fs afero.Fs, oldpath, newpath string) error {
	newpath = path.Clean(newpath)
	oldpath = path.Clean(oldpath)

	if !path.IsAbs(newpath) || !path.IsAbs(oldpath) {
		return vfs.ErrNonAbsolutePath
	}

	_, err := fs.Stat(newpath)
	if err == nil {
		return os.ErrExist
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	return fs.Rename(oldpath, newpath)
}

func safeRenameDir(afs *aferoVFS, oldpath, newpath string) error {
	newpath = path.Clean(newpath)
	oldpath = path.Clean(oldpath)

	if !path.IsAbs(newpath) || !path.IsAbs(oldpath) {
		return vfs.ErrNonAbsolutePath
	}

	if strings.HasPrefix(newpath, oldpath+"/") {
		return vfs.ErrForbiddenDocMove
	}

	_, err := afs.fs.Stat(newpath)
	if err == nil {
		return os.ErrExist
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	return afs.fs.Rename(oldpath, newpath)
}

var (
	_ vfs.VFS  = &aferoVFS{}
	_ vfs.File = &aferoFileOpen{}
	_ vfs.File = &aferoFileCreation{}
)