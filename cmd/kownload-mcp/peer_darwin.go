package main

import (
	"errors"
	"golang.org/x/sys/unix"
	"net"
	"os"
)

func checkPeer(conn net.Conn) error {
	return checkPeerForUID(conn, os.Getuid())
}

func checkPeerForUID(conn net.Conn, uid int) error {
	socket, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("not unix")
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return err
	}
	var credentialErr error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e != nil {
			credentialErr = e
			return
		}
		if int(cred.Uid) != uid {
			credentialErr = errors.New("peer owner mismatch")
		}
	})
	if err != nil {
		return err
	}
	return credentialErr
}
