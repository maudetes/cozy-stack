package apps

import (
	"compress/gzip"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/afero"
	"github.com/cozy/cozy-stack/pkg/magic"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/swift"
)

// Copier is an interface defining a common set of functions for the installer
// to copy the application into an unknown storage.
type Copier interface {
	Start(slug, version string) (exists bool, err error)
	Copy(stat os.FileInfo, src io.Reader) error
	Abort() error
	Commit() error
}

type swiftCopier struct {
	c         *swift.Connection
	appObj    string
	tmpObj    string
	container string
	started   bool
}

type aferoCopier struct {
	fs      afero.Fs
	appDir  string
	tmpDir  string
	started bool
}

// NewSwiftCopier defines a Copier storing data into a swift container.
func NewSwiftCopier(conn *swift.Connection, appsType AppType) Copier {
	return &swiftCopier{
		c:         conn,
		container: containerName(appsType),
	}
}

func (f *swiftCopier) Start(slug, version string) (bool, error) {
	f.appObj = path.Join(slug, version)
	_, _, err := f.c.Object(f.container, f.appObj)
	if err == nil {
		return true, nil
	}
	if err != swift.ObjectNotFound {
		return false, err
	}
	if _, _, err = f.c.Container(f.container); err == swift.ContainerNotFound {
		if err = f.c.ContainerCreate(f.container, nil); err != nil {
			return false, err
		}
	}
	f.tmpObj = "tmp-" + utils.RandomString(20) + "/"
	f.started = true
	return false, err
}

func (f *swiftCopier) Copy(stat os.FileInfo, src io.Reader) (err error) {
	if !f.started {
		panic("copier should call Start() before Copy()")
	}

	objName := path.Join(f.tmpObj, stat.Name())
	objMeta := swift.Metadata{
		"content-encoding":        "gzip",
		"original-content-length": strconv.FormatInt(stat.Size(), 10),
	}

	contentType := magic.MIMETypeByExtension(path.Ext(stat.Name()))
	if contentType == "" {
		contentType, src = magic.MIMETypeFromReader(src)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	file, err := f.c.ObjectCreate(f.container, objName, true, "",
		contentType, objMeta.ObjectHeaders())
	if err != nil {
		return err
	}
	defer func() {
		if errc := file.Close(); errc != nil {
			err = errc
		}
	}()

	gw, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		return err
	}
	defer func() {
		if errc := gw.Close(); errc != nil && err == nil {
			err = errc
		}
	}()

	_, err = io.Copy(gw, src)
	return err
}

func (f *swiftCopier) Abort() error {
	objectNames, err := f.c.ObjectNamesAll(f.container, &swift.ObjectsOpts{
		Prefix: f.tmpObj,
	})
	if err != nil {
		return err
	}
	_, err = f.c.BulkDelete(f.container, objectNames)
	return err
}

func (f *swiftCopier) Commit() error {
	objectNames, err := f.c.ObjectNamesAll(f.container, &swift.ObjectsOpts{
		Prefix: f.tmpObj,
	})
	if err != nil {
		return err
	}
	for _, srcObjectName := range objectNames {
		dstObjectName := path.Join(f.appObj, strings.TrimPrefix(srcObjectName, f.tmpObj))
		err = f.c.ObjectMove(f.container, srcObjectName, f.container, dstObjectName)
		if err != nil {
			return f.Abort()
		}
	}
	o, err := f.c.ObjectCreate(f.container, f.appObj, true, "", "", nil)
	if err != nil {
		return err
	}
	return o.Close()
}

// NewAferoCopier defines a copier using an afero.Fs filesystem to store the
// application data.
func NewAferoCopier(fs afero.Fs) Copier {
	return &aferoCopier{fs: fs}
}

func (f *aferoCopier) Start(slug, version string) (bool, error) {
	f.appDir = path.Join("/", slug, version)
	exists, err := afero.DirExists(f.fs, f.appDir)
	if err != nil || exists {
		return exists, err
	}
	dir := path.Dir(f.appDir)
	if err = f.fs.MkdirAll(dir, 0755); err != nil {
		return false, err
	}
	f.tmpDir, err = afero.TempDir(f.fs, dir, "tmp")
	if err != nil {
		return false, err
	}
	f.started = true
	return false, nil
}

func (f *aferoCopier) Copy(stat os.FileInfo, src io.Reader) (err error) {
	if !f.started {
		panic("copier should call Start() before Copy()")
	}

	fullpath := path.Join(f.tmpDir, stat.Name()) + ".gz"
	dir := path.Dir(fullpath)
	if err = f.fs.MkdirAll(dir, 0755); err != nil {
		return err
	}

	dst, err := f.fs.Create(fullpath)
	if err != nil {
		return err
	}
	defer func() {
		if errc := dst.Close(); errc != nil {
			err = errc
		}
	}()

	gw, err := gzip.NewWriterLevel(dst, gzip.BestCompression)
	if err != nil {
		return err
	}
	defer func() {
		if errc := gw.Close(); errc != nil && err == nil {
			err = errc
		}
	}()

	_, err = io.Copy(gw, src)
	return err
}

func (f *aferoCopier) Commit() error {
	return f.fs.Rename(f.tmpDir, f.appDir)
}

func (f *aferoCopier) Abort() error {
	return f.fs.RemoveAll(f.tmpDir)
}

type fileInfo struct {
	name string
	size int64
	mode os.FileMode
	time time.Time
}

func (f *fileInfo) Name() string       { return f.name }
func (f *fileInfo) Size() int64        { return f.size }
func (f *fileInfo) Mode() os.FileMode  { return f.mode }
func (f *fileInfo) ModTime() time.Time { return f.time }
func (f *fileInfo) IsDir() bool        { return false }
func (f *fileInfo) Sys() interface{}   { return nil }
