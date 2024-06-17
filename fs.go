package adbfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"
)

// https://cs.android.com/android/platform/superproject/main/+/main:packages/modules/adb/file_sync_protocol.h;drc=888a54dcbf954fdffacc8283a793290abcc589cd
// https://cs.android.com/android/_/android/platform/packages/modules/adb/+/f354ebb4a8a928e3b1e50b19ed9030431825212b:file_sync_service.h
// https://cs.android.com/android/platform/superproject/main/+/main:packages/modules/adb/daemon/file_sync_service.cpp;drc=888a54dcbf954fdffacc8283a793290abcc589cd
// https://cs.android.com/android/platform/superproject/main/+/main:packages/modules/adb/client/file_sync_client.cpp;drc=d5137445c0d4067406cb3e38aade5507ff2fcd16;bpv=0;bpt=0
// https://cs.android.com/android/platform/superproject/main/+/main:packages/modules/adb/protocol.txt;drc=ebf09dd6e6cf295df224730b1551606c521e74a9
// https://cs.android.com/android/platform/superproject/main/+/main:packages/modules/adb/SERVICES.txt;drc=ebf09dd6e6cf295df224730b1551606c521e74a9
// https://cs.android.com/android/platform/superproject/main/+/main:packages/modules/adb/adb.cpp;drc=ebf09dd6e6cf295df224730b1551606c521e74a9
// https://gist.github.com/hfutxqd/a5b2969c485dabd512e543768a35a046
// https://github.com/cstyan/adbDocumentation
// note: sync STA2/LST2 since 2016, LIS2 since 2019

var (
	errNotDirectory = errors.New("not a directory")
	errIsDirectory  = errors.New("is a directory")
)

// FS provides access to the filesystem of an ADB device.
//
// A pool of connections is used. Additional connections will be opened for
// concurrent operations, or when other operations are done while a file is open
// for reading.
//
// In general, fs.ReadFile, fs.ReadDir, and fs.Stat should be used over the Open
// method since Open always does a stat. If Open is used (e.g., for streaming
// large files), it should be read fully and closed as soon as possible to
// prevent additional connections from being opened unnecessarily.
type FS struct {
	addr   string
	serial string
	feat   []string
	connMu sync.Mutex
	conn   map[net.Conn]bool // [conn]used
}

var (
	_ fs.FS         = (*FS)(nil)
	_ fs.StatFS     = (*FS)(nil)
	_ fs.ReadDirFS  = (*FS)(nil)
	_ fs.ReadFileFS = (*FS)(nil)
)

// Connect connects to the specified serial (or any device if empty) via the ADB
// daemon at addr.
func Connect(addr, serial string) (*FS, error) {
	if serial == "" {
		buf, err := adbConnectSingle(addr, "host:get-serialno")
		if err != nil {
			return nil, fmt.Errorf("get any device serial number: %w", err)
		}
		serial = string(buf)
	}

	buf, err := adbConnectSingle(addr, "host-serial:"+serial+":features")
	if err != nil {
		return nil, fmt.Errorf("get device features: %w", err)
	}
	feat := strings.Split(string(buf), ",")

	c := &FS{
		addr:   addr,
		serial: serial,
		feat:   feat,
		conn:   make(map[net.Conn]bool),
	}

	runtime.SetFinalizer(c, func(f *FS) {
		f.Close()
	})

	conn, err := c.getConn()
	if err != nil {
		return nil, fmt.Errorf("connect to sync service: %w", err)
	}
	defer c.putConn(conn)

	return c, nil
}

func (c *FS) getConn() (net.Conn, error) {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn == nil {
		return nil, fs.ErrClosed
	}

	for conn, free := range c.conn {
		if free {
			c.conn[conn] = false
			return conn, nil
		}
	}

	conn, err := adbConnectDevice(c.addr, c.serial, "sync:")
	if err != nil {
		return nil, fmt.Errorf("connect to sync service: %w", err)
	}
	c.conn[conn] = false

	return conn, nil
}

func (c *FS) putConn(conn net.Conn) {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if _, ok := c.conn[conn]; ok {
		c.conn[conn] = true
	}
}

func (c *FS) delConn(conn net.Conn) {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	conn.Close()
	delete(c.conn, conn)
}

func (c *FS) Close() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	for conn := range c.conn {
		conn.Close()
		delete(c.conn, conn)
	}
	c.conn = nil

	return nil
}

func (c *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{
			Op:   "open",
			Path: name,
			Err:  fs.ErrInvalid,
		}
	}

	conn, err := c.getConn()
	if err != nil {
		return nil, err
	}

	keepConn := false
	defer func() {
		if !keepConn {
			c.putConn(conn)
		}
	}()

	if err := syncRequest(conn, syncID_LSTAT_V1, "/"+name); err != nil {
		return nil, &fs.PathError{
			Op:   "stat",
			Path: name,
			Err:  err,
		}
	}
	st, err := syncResponseObject[sync_stat_v1](conn, syncID_LSTAT_V1)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "stat",
			Path: name,
			Err:  err,
		}
	}
	if *st == (sync_stat_v1{}) {
		return nil, &fs.PathError{
			Op:   "stat",
			Path: name,
			Err:  fmt.Errorf("%w (or permission denied)", fs.ErrNotExist), // we have no way to tell from here with v1
		}
	}

	f := &fsFile{c: c, name: name, st: st}
	if !syncMode(st.Mode).IsDir() {
		id := syncID_RECV_V1
		if err := syncRequest(conn, id, "/"+name); err != nil {
			return nil, &fs.PathError{
				Op:   "open",
				Path: name,
				Err:  fmt.Errorf("do %s: %w", id, err),
			}
		}
		f.conn, keepConn = conn, true
	}
	return f, nil
}

func (c *FS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{
			Op:   "open",
			Path: name,
			Err:  fs.ErrInvalid,
		}
	}

	conn, err := c.getConn()
	if err != nil {
		return nil, err
	}
	defer c.putConn(conn)

	return fsStat(conn, name)
}

func fsStat(conn net.Conn, name string) (fs.FileInfo, error) {
	if err := syncRequest(conn, syncID_LSTAT_V1, "/"+name); err != nil {
		return nil, &fs.PathError{
			Op:   "stat",
			Path: name,
			Err:  err,
		}
	}
	st, err := syncResponseObject[sync_stat_v1](conn, syncID_LSTAT_V1)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "stat",
			Path: name,
			Err:  err,
		}
	}
	if *st == (sync_stat_v1{}) {
		return nil, &fs.PathError{
			Op:   "stat",
			Path: name,
			Err:  fmt.Errorf("%w (or permission denied)", fs.ErrNotExist), // we have no way to tell from here with v1
		}
	}
	return &fsFileInfo{name: path.Base(name), st: st}, nil
}

func (c *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{
			Op:   "open",
			Path: name,
			Err:  fs.ErrInvalid,
		}
	}

	conn, err := c.getConn()
	if err != nil {
		return nil, err
	}
	defer c.putConn(conn)

	return fsReadDir(conn, name)
}

func fsReadDir(conn net.Conn, name string) ([]fs.DirEntry, error) {
	if err := syncRequest(conn, syncID_LIST_V1, "/"+name); err != nil {
		return nil, &fs.PathError{
			Op:   "readdir",
			Path: name,
			Err:  err,
		}
	}

	var de []fs.DirEntry
	var seen bool
	for {
		st, err := syncResponseObject[sync_dent_v1](conn, syncID_DENT_V1)
		if err != nil {
			return nil, &fs.PathError{
				Op:   "readdirent",
				Path: name,
				Err:  err,
			}
		}
		if st == nil {
			if !seen {
				if st, err := fsStat(conn, name); err != nil {
					if err, ok := err.(*fs.PathError); ok {
						err.Op = "readdirent"
						return nil, err
					}
					return nil, err
				} else if !st.IsDir() {
					return nil, &fs.PathError{
						Op:   "readdirent",
						Path: name,
						Err:  errNotDirectory,
					}
				}
				// could be an empty directory or not found, no way to tell reliably with v1
			}
			break
		} else {
			seen = true
		}
		nb := make([]byte, st.Namelen)
		if _, err := io.ReadFull(conn, nb); err != nil {
			return nil, &fs.PathError{
				Op:   "readdirentname",
				Path: name,
				Err:  err,
			}
		}
		if string(nb) == "." || string(nb) == ".." {
			continue
		}
		de = append(de, &fsDirEntry{name: string(nb), st: st})
	}
	return de, nil
}

func (c *FS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{
			Op:   "open",
			Path: name,
			Err:  fs.ErrInvalid,
		}
	}

	conn, err := c.getConn()
	if err != nil {
		return nil, err
	}
	defer c.putConn(conn)

	if err := syncRequest(conn, syncID_RECV_V1, "/"+name); err != nil {
		return nil, &fs.PathError{
			Op:   "readfile",
			Path: name,
			Err:  err,
		}
	}

	var buf bytes.Buffer
	for {
		// get another chunk
		st, err := syncResponseObject[sync_data](conn, syncID_DATA)
		if err != nil {
			c.delConn(conn)
			return nil, &fs.PathError{
				Op:   "readfile",
				Path: name,
				Err:  err,
			}
		}

		// check if we don't have any chunks left
		if st == nil {
			break
		}

		// read a chunk
		buf.Grow(int(st.Size))
		if _, err := io.ReadFull(conn, buf.AvailableBuffer()[:st.Size]); err != nil {
			c.delConn(conn)
			return nil, &fs.PathError{
				Op:   "readfile",
				Path: name,
				Err:  err,
			}
		} else {
			buf.Write(buf.AvailableBuffer()[:st.Size])
		}
	}
	return buf.Bytes(), nil
}

type fsFile struct {
	c    *FS
	name string
	st   *sync_stat_v1

	mu   sync.Mutex
	conn net.Conn
	buf  bytes.Buffer
	er   error
}

func (f *fsFile) Stat() (fs.FileInfo, error) {
	return &fsFileInfo{name: path.Base(f.name), st: f.st}, nil
}

func (f *fsFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if syncMode(f.st.Mode).IsDir() {
		return 0, &fs.PathError{
			Op:   "read",
			Path: f.name,
			Err:  errIsDirectory,
		}
	}
	if f.er != nil {
		return 0, f.er
	}

	if f.buf.Len() == 0 {
		// get another chunk
		st, err := syncResponseObject[sync_data](f.conn, syncID_DATA)
		if err != nil {
			f.c.delConn(f.conn)
			f.conn = nil
			f.er = &fs.PathError{
				Op:   "read",
				Path: f.name,
				Err:  err,
			}
			return 0, f.er
		}

		// check if we don't have any chunks left
		if st == nil {
			f.c.putConn(f.conn)
			f.conn = nil
			f.er = io.EOF
			return 0, f.er
		}

		// read a chunk
		f.buf.Grow(int(st.Size))
		if _, err := io.ReadFull(f.conn, f.buf.AvailableBuffer()[:st.Size]); err != nil {
			f.c.delConn(f.conn)
			f.conn = nil
			f.er = &fs.PathError{
				Op:   "read",
				Path: f.name,
				Err:  err,
			}
			return 0, f.er
		} else {
			f.buf.Write(f.buf.AvailableBuffer()[:st.Size])
		}
	}

	// read from our buffered chunk
	return f.buf.Read(p)
}

func (f *fsFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !syncMode(f.st.Mode).IsDir() {
		return nil, &fs.PathError{
			Op:   "readdir",
			Path: f.name,
			Err:  errNotDirectory,
		}
	}
	return f.c.ReadDir(f.name)
}

func (f *fsFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.conn != nil {
		f.c.delConn(f.conn) // don't put a conn in a bad state back
		f.conn = nil
		f.er = fs.ErrClosed
	}
	return nil
}

type fsFileInfo struct {
	name string
	st   *sync_stat_v1
}

func (f *fsFileInfo) Name() string {
	return f.name
}

func (f *fsFileInfo) Size() int64 {
	return int64(f.st.Size)
}

func (f *fsFileInfo) Mode() fs.FileMode {
	return syncMode(f.st.Mode)
}

func (f *fsFileInfo) ModTime() time.Time {
	return time.Unix(int64(f.st.Mtime), 0)
}

func (f *fsFileInfo) IsDir() bool {
	return f.Mode().IsDir()
}

func (f *fsFileInfo) Sys() any {
	return nil
}

type fsDirEntry struct {
	name string
	st   *sync_dent_v1
}

func (f *fsDirEntry) Name() string {
	return f.name
}

func (f *fsDirEntry) IsDir() bool {
	return f.Mode().IsDir()
}

func (f *fsDirEntry) Type() fs.FileMode {
	return syncMode(f.st.Mode).Type()
}

func (f *fsDirEntry) Info() (fs.FileInfo, error) {
	return f, nil
}

func (f *fsDirEntry) Size() int64 {
	return int64(f.st.Size)
}

func (f *fsDirEntry) Mode() fs.FileMode {
	return syncMode(f.st.Mode)
}

func (f *fsDirEntry) ModTime() time.Time {
	return time.Unix(int64(f.st.Mtime), 0)
}

func (f *fsDirEntry) Sys() any {
	return nil
}
