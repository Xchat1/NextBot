package main

import "syscall"

func peerUID(fd int) (uint32, error) {
	peer, err := syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return peer.Uid, nil
}
