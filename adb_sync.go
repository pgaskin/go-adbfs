package adbfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strings"
)

const (
	syncFeature_stat_v2                  string = "stat_v2"
	syncFeature_ls_v2                    string = "ls_v2"
	syncFeature_sendrecv_v2              string = "sendrecv_v2"
	syncFeature_sendrecv_v2_brotli       string = "sendrecv_v2_brotli"
	syncFeature_sendrecv_v2_lz4          string = "sendrecv_v2_lz4"
	syncFeature_sendrecv_v2_zstd         string = "sendrecv_v2_zstd"
	syncFeature_sendrecv_v2_dry_run_send string = "sendrecv_v2_dry_run_send"
)

type syncID [4]byte

var (
	syncID_LSTAT_V1 = syncID{'S', 'T', 'A', 'T'}
	syncID_STAT_V2  = syncID{'S', 'T', 'A', '2'} // if syncFeature_stat_v2
	syncID_LSTAT_V2 = syncID{'L', 'S', 'T', '2'} // if syncFeature_stat_v2
	syncID_LIST_V1  = syncID{'L', 'I', 'S', 'T'}
	syncID_LIST_V2  = syncID{'L', 'I', 'S', '2'} // if syncFeature_ls_v2
	syncID_DENT_V1  = syncID{'D', 'E', 'N', 'T'}
	syncID_DENT_V2  = syncID{'D', 'N', 'T', '2'} // if syncFeature_ls_v2
	syncID_SEND_V1  = syncID{'S', 'E', 'N', 'D'}
	syncID_SEND_V2  = syncID{'S', 'N', 'D', '2'} // if syncFeature_sendrecv_v2
	syncID_RECV_V1  = syncID{'R', 'E', 'C', 'V'}
	syncID_RECV_V2  = syncID{'R', 'C', 'V', '2'} // if syncFeature_sendrecv_v2
	syncID_DONE     = syncID{'D', 'O', 'N', 'E'} // signals the end of an array of values
	syncID_DATA     = syncID{'D', 'A', 'T', 'A'}
	syncID_OKAY     = syncID{'O', 'K', 'A', 'Y'}
	syncID_FAIL     = syncID{'F', 'A', 'I', 'L'}
	syncID_QUIT     = syncID{'Q', 'U', 'I', 'T'}
)

func (id syncID) String() string {
	return string(id[:])
}

type syncFail string

func (s syncFail) Error() string {
	return "sync error: " + string(s)
}

const syncDataMax = 64 * 1024

func syncRequest(conn net.Conn, id syncID, path string) error {
	req := make([]byte, 4+4+len(path))
	copy(req[0:4], id[:])
	binary.LittleEndian.PutUint32(req[4:8], uint32(len(path)))
	copy(req[8:], path)
	_, err := conn.Write(req)
	return err
}

func syncResponse(conn net.Conn) error {
	b := make([]byte, 4)
	if _, err := io.ReadFull(conn, b); err != nil {
		return fmt.Errorf("read response id: %w", err)
	}
	if err := syncResponseCheck(conn, syncID(b)); err != nil {
		return err
	}
	if id := syncID_OKAY; syncID(b) != id {
		return fmt.Errorf("unexpected response id %q (expected %s)", syncID(b), id)
	}
	return nil
}

func syncResponseObject[T any](conn net.Conn, id syncID) (*T, error) {
	var obj T
	b := make([]byte, 4)
	if _, err := io.ReadFull(conn, b); err != nil {
		return nil, fmt.Errorf("read response id: %w", err)
	}
	if err := syncResponseCheck(conn, syncID(b)); err != nil {
		return nil, err
	}
	if syncID(b) != id && syncID(b) != syncID_DONE {
		return nil, fmt.Errorf("unexpected response id %q (expected %s)", syncID(b), id)
	}
	if err := binary.Read(conn, binary.LittleEndian, &obj); err != nil {
		return nil, fmt.Errorf("read response %s: %w", id, err)
	}
	if syncID(b) == syncID_DONE {
		return nil, nil
	}
	return &obj, nil
}

func syncResponseCheck(conn net.Conn, id syncID) error {
	switch id {
	case syncID_FAIL:
		var tmp sync_status
		if err := binary.Read(conn, binary.LittleEndian, &tmp); err != nil {
			return fmt.Errorf("read error response: %w", err)
		}
		tmp1 := make([]byte, tmp.Msglen)
		if _, err := io.ReadFull(conn, tmp1); err != nil {
			return fmt.Errorf("read error response: %w", err)
		}
		msg := string(tmp1)
		switch { // libc error strings
		case strings.HasSuffix(msg, ": Is a directory"):
			return errIsDirectory
		case strings.HasSuffix(msg, ": Not a directory"):
			return errNotDirectory
		case strings.HasSuffix(msg, ": Permission denied"):
			return fs.ErrPermission
		case strings.HasSuffix(msg, ": No such file or directory"):
			return fs.ErrNotExist
		}
		return syncFail(msg)
	case syncID_OKAY:
		var tmp sync_status
		if err := binary.Read(conn, binary.LittleEndian, &tmp); err != nil {
			return fmt.Errorf("read okay response: %w", err)
		}
		if tmp.Msglen != 0 {
			return fmt.Errorf("read okay response: message length must be zero, got %d", tmp.Msglen)
		}
	}
	return nil
}

type sync_stat_v1 struct {
	// syncID_LSTAT_V1
	Mode  uint32
	Size  uint32
	Mtime uint32
}

type sync_stat_v2 struct {
	// syncID_STAT_V2, syncID_LSTAT_V2
	Error uint32
	Dev   uint64
	Ino   uint64
	Mode  uint32
	Nlink uint32
	Uid   uint32
	Gid   uint32
	Size  uint64
	Atime int64
	Mtime int64
	Ctime int64
}

type sync_dent_v1 struct {
	// syncID_DENT_V1
	Mode    uint32
	Size    uint32
	Mtime   uint32
	Namelen uint32
	// followed by `namelen` bytes of the name.
}

type sync_dent_v2 struct {
	// syncID_DENT_V2
	Error   uint32
	Dev     uint64
	Ino     uint64
	Mode    uint32
	Nlink   uint32
	Uid     uint32
	Gid     uint32
	Size    uint64
	Atime   int64
	Mtime   int64
	Ctime   int64
	Namelen uint32
	// followed by `namelen` bytes of the name.
}

const (
	syncFlag_None   uint32 = 0
	syncFlag_Brotli uint32 = 1          // if syncFeature_sendrecv_v2_brotli
	syncFlag_LZ4    uint32 = 2          // if syncFeature_sendrecv_v2_lz4
	syncFlag_Zstd   uint32 = 4          // if syncFeature_sendrecv_v2_zstd
	syncFlag_DryRun uint32 = 0x80000000 // if syncFeature_sendrecv_v2_dry_run_send
)

// send_v1 sent the path in a buffer, followed by a comma and the mode as a string.

// send_v2 sends just the path in the first request, and then sends another with details.
type sync_send_v2 struct {
	// syncID_SEND_V2
	Mode  uint32
	Flags uint32
}

// recv_v1 just sent the path without any accompanying data.

// recv_v2 sends just the path in the first request, and then sends another with details.
type sync_recv_v2 struct {
	// syncID_RECV_V2
	Flags uint32
}

type sync_data struct {
	// syncID_DATA
	Size uint32
	// followed by `size` bytes of data.
}

type sync_status struct {
	// syncID_OKAY, syncID_FAIL, syncID_DONE
	Msglen uint32
	// followed by `msglen` bytes of error message, if id == ID_FAIL.
}

func syncMode(mode uint32) fs.FileMode {
	const (
		S_BLKSIZE = 0x200
		S_IEXEC   = 0x40
		S_IFBLK   = 0x6000
		S_IFCHR   = 0x2000
		S_IFDIR   = 0x4000
		S_IFIFO   = 0x1000
		S_IFLNK   = 0xa000
		S_IFMT    = 0xf000
		S_IFREG   = 0x8000
		S_IFSOCK  = 0xc000
		S_IREAD   = 0x100
		S_IRGRP   = 0x20
		S_IROTH   = 0x4
		S_IRUSR   = 0x100
		S_IRWXG   = 0x38
		S_IRWXO   = 0x7
		S_IRWXU   = 0x1c0
		S_ISGID   = 0x400
		S_ISUID   = 0x800
		S_ISVTX   = 0x200
		S_IWGRP   = 0x10
		S_IWOTH   = 0x2
		S_IWRITE  = 0x80
		S_IWUSR   = 0x80
		S_IXGRP   = 0x8
		S_IXOTH   = 0x1
		S_IXUSR   = 0x40
	)
	m := fs.FileMode(mode & 0777)
	switch mode & S_IFMT {
	case S_IFBLK:
		m |= fs.ModeDevice
	case S_IFCHR:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case S_IFDIR:
		m |= fs.ModeDir
	case S_IFIFO:
		m |= fs.ModeNamedPipe
	case S_IFLNK:
		m |= fs.ModeSymlink
	case S_IFREG:
		// nothing to do
	case S_IFSOCK:
		m |= fs.ModeSocket
	}
	if mode&S_ISGID != 0 {
		m |= fs.ModeSetgid
	}
	if mode&S_ISUID != 0 {
		m |= fs.ModeSetuid
	}
	if mode&S_ISVTX != 0 {
		m |= fs.ModeSticky
	}
	return m
}
