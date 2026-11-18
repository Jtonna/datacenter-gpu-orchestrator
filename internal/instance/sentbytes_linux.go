//go:build linux

package instance

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Counting what a child has uploaded.
//
// /proc/<pid>/io is not usable: its wchar counts write(2) but not send(2),
// sendto(2) or sendmsg(2), and llama.cpp's RPC client uses send() (shown by
// TestProcIOWcharMissesSend). Instead we ask the kernel (netlink sock_diag,
// the same source as `ss -ti`) for the TCP_INFO of every TCP socket whose
// peer is one of the workers and which belongs to the child's process group,
// and add up bytes_acked: the bytes the worker has acknowledged, which is
// what has really crossed the wire.
//
// The scheduler and its children share a network namespace, so the
// scheduler's own netlink socket sees the children's sockets.

const (
	nlmsgDone       = 3
	nlmsgError      = 2
	sockDiagByFam   = 20 // SOCK_DIAG_BY_FAMILY
	inetDiagInfo    = 2  // INET_DIAG_INFO attribute: struct tcp_info
	inetDiagReqSize = 56 // struct inet_diag_req_v2
	inetDiagMsgSize = 72 // struct inet_diag_msg
	tcpInfoAckedOff = 120
)

func socketBytesAcked(pid int, peers []string) (SentCount, error) {
	want := map[netip.AddrPort]string{}
	for _, p := range peers {
		ap, err := netip.ParseAddrPort(p)
		if err != nil {
			continue
		}
		want[netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())] = p
	}
	if len(want) == 0 {
		return SentCount{}, errors.New("no worker addresses to count")
	}
	inodes, err := groupSocketInodes(pid)
	if err != nil {
		return SentCount{}, err
	}
	if len(inodes) == 0 {
		return SentCount{}, nil
	}
	out := SentCount{Peers: map[string]int64{}}
	var errs []error
	fams := 0
	for _, fam := range []uint8{syscall.AF_INET, syscall.AF_INET6} {
		got, err := diagFamily(fam)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fams++
		for _, s := range got {
			if name, ok := want[s.peer]; ok && inodes[s.inode] {
				out.Bytes += s.acked
				out.Sockets++
				out.Peers[name] += s.acked
			}
		}
	}
	if fams == 0 {
		return SentCount{}, errors.Join(errs...)
	}
	return out, nil
}

// groupSocketInodes lists the socket inodes held by the process pid and by
// every process in its process group (the child runs in its own group, and a
// wrapper script may have started the real server).
func groupSocketInodes(pid int) (map[uint32]bool, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := map[uint32]bool{}
	found := false
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if n != pid && pgrpOf(n) != pid {
			continue
		}
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", n))
		if err != nil {
			if n == pid {
				return nil, err
			}
			continue
		}
		found = true
		for _, fd := range fds {
			link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", n, fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			if ino, err := strconv.ParseUint(strings.TrimSuffix(link[len("socket:["):], "]"), 10, 32); err == nil {
				out[uint32(ino)] = true
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("process %d not found", pid)
	}
	return out, nil
}

// pgrpOf reads a process's group from /proc/<n>/stat, or -1.
func pgrpOf(n int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", n))
	if err != nil {
		return -1
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return -1
	}
	f := strings.Fields(s[i+1:]) // state ppid pgrp ...
	if len(f) < 3 {
		return -1
	}
	g, err := strconv.Atoi(f[2])
	if err != nil {
		return -1
	}
	return g
}

type diagSock struct {
	peer  netip.AddrPort
	inode uint32
	acked int64
}

// diagFamily dumps every TCP socket of one address family with its tcp_info.
func diagFamily(family uint8) ([]diagSock, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_INET_DIAG)
	if err != nil {
		return nil, fmt.Errorf("sock_diag: %w", err)
	}
	defer syscall.Close(fd)
	tv := syscall.Timeval{Sec: 2}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return nil, fmt.Errorf("sock_diag: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("sock_diag: %w", err)
	}

	req := make([]byte, syscall.NLMSG_HDRLEN+inetDiagReqSize)
	native := binary.NativeEndian
	native.PutUint32(req[0:], uint32(len(req)))
	native.PutUint16(req[4:], sockDiagByFam)
	native.PutUint16(req[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP)
	native.PutUint32(req[8:], 1)
	b := req[syscall.NLMSG_HDRLEN:]
	b[0] = family
	b[1] = syscall.IPPROTO_TCP
	b[2] = 1 << (inetDiagInfo - 1) // ask for INET_DIAG_INFO
	native.PutUint32(b[4:], 0xffffffff)
	if err := syscall.Sendto(fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("sock_diag: %w", err)
	}

	var out []diagSock
	buf := make([]byte, 1<<16)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, fmt.Errorf("sock_diag: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, fmt.Errorf("sock_diag: %w", err)
		}
		for _, m := range msgs {
			switch m.Header.Type {
			case nlmsgDone:
				return out, nil
			case nlmsgError:
				return nil, errors.New("sock_diag: the kernel refused the request")
			}
			if s, ok := parseDiagMsg(m.Data); ok {
				out = append(out, s)
			}
		}
	}
}

// parseDiagMsg reads one inet_diag_msg followed by its attributes.
func parseDiagMsg(d []byte) (diagSock, bool) {
	if len(d) < inetDiagMsgSize {
		return diagSock{}, false
	}
	native := binary.NativeEndian
	family := d[0]
	dport := binary.BigEndian.Uint16(d[6:8]) // id.idiag_dport
	var dst netip.Addr
	if family == syscall.AF_INET {
		dst = netip.AddrFrom4([4]byte(d[24:28])) // id.idiag_dst
	} else {
		dst = netip.AddrFrom16([16]byte(d[24:40])).Unmap()
	}
	s := diagSock{peer: netip.AddrPortFrom(dst, dport), inode: native.Uint32(d[68:72])}
	attrs := d[inetDiagMsgSize:]
	for len(attrs) >= 4 {
		l := int(native.Uint16(attrs[0:2]))
		typ := native.Uint16(attrs[2:4])
		if l < 4 || l > len(attrs) {
			break
		}
		if typ == inetDiagInfo && l >= 4+tcpInfoAckedOff+8 {
			s.acked = int64(native.Uint64(attrs[4+tcpInfoAckedOff:]))
		}
		next := (l + 3) &^ 3
		if next > len(attrs) {
			break
		}
		attrs = attrs[next:]
	}
	return s, true
}
