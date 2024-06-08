package adbfs

import (
	"fmt"
	"io"
	"net"
	"strconv"
)

func adbConnect(addr, svc string) (net.Conn, error) {
	if addr == "" {
		addr = "localhost:5037"
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect %q: %w", addr, err)
	}
	if err := adbService(conn, svc); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func adbConnectSingle(addr, svc string) ([]byte, error) {
	conn, err := adbConnect(addr, svc)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if buf, err := adbRecvMsg(conn); err != nil {
		return nil, fmt.Errorf("service %q: recv message: %w", svc, err)
	} else {
		return buf, nil
	}
}

func adbConnectDevice(addr, serial, svc string) (net.Conn, error) {
	var svc1 string
	if serial != "" {
		svc1 = "host:transport:" + serial
	} else {
		svc1 = "host:transport-any"
	}
	conn, err := adbConnect(addr, svc1)
	if err != nil {
		return nil, err
	}
	if err := adbService(conn, svc); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func adbService(conn net.Conn, svc string) error {
	if err := adbSendMsg(conn, svc); err != nil {
		return fmt.Errorf("service %q: send message: %w", svc, err)
	}
	if status, err := adbRecvStatus(conn); err != nil {
		return fmt.Errorf("service %q: recv status: %w", svc, err)
	} else if status != "OKAY" {
		return fmt.Errorf("service %q: adb status %q", svc, status)
	}
	return nil
}

func adbSendMsg(conn net.Conn, msg string) error {
	_, err := conn.Write([]byte(fmt.Sprintf("%04x%s", len(msg), msg)))
	return err
}

func adbRecvStatus(conn net.Conn) (string, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(conn, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func adbRecvMsg(conn net.Conn) ([]byte, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(conn, b); err != nil {
		return nil, err
	}
	n, err := strconv.ParseUint(string(b), 16, 32)
	if err != nil {
		return nil, err
	}
	b = make([]byte, int(n))
	if _, err := io.ReadFull(conn, b); err != nil {
		return nil, err
	}
	return b, nil
}
