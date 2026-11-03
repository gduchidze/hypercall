//go:build linux

package guest

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func readCloneSession() (_ *cloneSession, resultErr error) {
	// /dev/vdb is never mounted while capturing. Invalidate inherited kernel block
	// cache before reading the private replacement backing disk after restoration.
	disk, err := os.OpenFile("/dev/vdb", os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer disk.Close()
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, disk.Fd(), 0x1261, 0); errno != 0 {
		return nil, errno
	}
	const target = "/run/hypercall/session"
	if err = syscall.Mount("/dev/vdb", target, "ext4", syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "noload"); err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, syscall.Unmount(target, 0)) }()
	f, err := os.OpenFile(target+"/session.json", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > 16384 {
		return nil, errors.New("invalid session disk file")
	}
	d := json.NewDecoder(io.LimitReader(f, 16385))
	d.DisallowUnknownFields()
	var s cloneSession
	if err = d.Decode(&s); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid session JSON")
	}
	return &s, nil
}
func reseedKernel(entropy []byte) error {
	if len(entropy) != 32 {
		return errors.New("256 entropy bits required")
	}
	f, err := os.OpenFile("/dev/random", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	pool := struct {
		Bits int32
		Size int32
		Data [32]byte
	}{Bits: 256, Size: 32}
	copy(pool.Data[:], entropy)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x40085203, uintptr(unsafe.Pointer(&pool))); errno != 0 {
		return errno
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x5207, 0); errno != 0 {
		return errno
	}
	return nil
}

func syncGuestDisks() { syscall.Sync() }

func setGuestClock(hostUnixNano int64) error {
	stamp := unix.NsecToTimespec(hostUnixNano)
	return unix.ClockSettime(unix.CLOCK_REALTIME, &stamp)
}
