package main

// #include <unistd.h>
import "C"

func peerUID(fd int) (uint32, error) {
	var uid C.uid_t
	var gid C.gid_t
	if result, err := C.getpeereid(C.int(fd), &uid, &gid); result != 0 {
		return 0, err
	}
	return uint32(uid), nil
}
