package gost

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/go-log/log"
	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/songgao/water"
	"golang.org/x/net/ipv4"
)

type TunConfig struct {
	Name   string
	Addr   string
	MTU    int
	Routes []string
}

type tunHandler struct {
	raddr   string
	options *HandlerOptions
}

// TunHandler creates a handler for tun tunnel.
func TunHandler(raddr string, opts ...HandlerOption) Handler {
	h := &tunHandler{
		raddr:   raddr,
		options: &HandlerOptions{},
	}
	for _, opt := range opts {
		opt(h.options)
	}

	return h
}

func (h *tunHandler) Init(options ...HandlerOption) {
	if h.options == nil {
		h.options = &HandlerOptions{}
	}
	for _, opt := range options {
		opt(h.options)
	}
}

func (h *tunHandler) Handle(conn net.Conn) {
	defer os.Exit(0)
	defer conn.Close()

	uc, ok := conn.(net.PacketConn)
	if !ok {
		log.Log("[tun] wrong connection type, must be PacketConn")
		return
	}

	tc, err := h.createTun()
	if err != nil {
		log.Logf("[tun] %s create tun: %v", conn.LocalAddr(), err)
		return
	}
	defer tc.Close()

	log.Logf("[tun] %s - %s: tun creation successful", tc.LocalAddr(), conn.LocalAddr())

	var raddr net.Addr
	if h.raddr != "" {
		raddr, err = net.ResolveUDPAddr("udp", h.raddr)
		if err != nil {
			log.Logf("[tun] %s - %s remote addr: %v", tc.LocalAddr(), conn.LocalAddr(), err)
			return
		}
	}

	if len(h.options.Users) > 0 && h.options.Users[0] != nil {
		passwd, _ := h.options.Users[0].Password()
		cipher, err := core.PickCipher(h.options.Users[0].Username(), nil, passwd)
		if err != nil {
			log.Logf("[tun] %s - %s cipher: %v", tc.LocalAddr(), conn.LocalAddr(), err)
			return
		}
		uc = cipher.PacketConn(uc)
	}

	h.transportTun(tc, uc, raddr)
}

func (h *tunHandler) createTun() (conn net.Conn, err error) {
	cfg := h.options.TunConfig

	ip, _, err := net.ParseCIDR(cfg.Addr)
	if err != nil {
		return
	}

	ifce, err := water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Name: cfg.Name,
		},
	})
	if err != nil {
		return
	}

	setup := func(args ...string) error {
		cmd := exec.Command("/sbin/ip", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = DefaultMTU
	}

	if err = setup("link", "set", "dev", ifce.Name(), "mtu", strconv.Itoa(mtu)); err != nil {
		return
	}
	if err = setup("addr", "add", cfg.Addr, "dev", ifce.Name()); err != nil {
		return
	}
	if err = setup("link", "set", "dev", ifce.Name(), "up"); err != nil {
		return
	}

	tc := &tunConn{
		ifce: ifce,
		addr: &net.IPAddr{IP: ip},
	}
	return tc, nil
}

func (h *tunHandler) transportTun(tun net.Conn, conn net.PacketConn, raddr net.Addr) error {
	var routes sync.Map
	errc := make(chan error, 1)

	go func() {
		for {
			err := func() error {
				b := sPool.Get().([]byte)
				defer sPool.Put(b)

				n, err := tun.Read(b)
				if err != nil {
					return err
				}

				header, err := ipv4.ParseHeader(b[:n])
				if err != nil {
					log.Logf("[tun] %s: %v", tun.LocalAddr(), err)
					return err
				}

				if header.Version != ipv4.Version {
					log.Logf("[tun] %s: v%d ignored, only support ipv4",
						tun.LocalAddr(), header.Version)
					return nil
				}

				addr := raddr
				if v, ok := routes.Load(header.Dst.String()); ok {
					addr = v.(net.Addr)
				}
				if addr == nil {
					log.Logf("[tun] %s: no address to forward for %s -> %s",
						tun.LocalAddr(), header.Src, header.Dst)
					return nil
				}

				if Debug {
					log.Logf("[tun] %s >>> %s: %s -> %s %d/%d %x %x %d",
						tun.LocalAddr(), addr, header.Src, header.Dst,
						header.Len, header.TotalLen, header.ID, header.Flags, header.Protocol)
				}

				if _, err := conn.WriteTo(b[:n], addr); err != nil {
					return err
				}
				return nil
			}()

			if err != nil {
				errc <- err
				return
			}
		}
	}()

	go func() {
		for {
			err := func() error {
				b := sPool.Get().([]byte)
				defer mPool.Put(b)

				n, addr, err := conn.ReadFrom(b)
				if err != nil {
					return err
				}

				header, err := ipv4.ParseHeader(b[:n])
				if err != nil {
					log.Logf("[tun] %s <- %s: %v", tun.LocalAddr(), addr, err)
					return err
				}

				if header.Version != ipv4.Version {
					log.Logf("[tun] %s <- %s: v%d ignored, only support ipv4",
						tun.LocalAddr(), addr, header.Version)
					return nil
				}

				if Debug {
					log.Logf("[tun] %s <<< %s: %s -> %s %d/%d %x %x %d",
						tun.LocalAddr(), addr, header.Src, header.Dst,
						header.Len, header.TotalLen, header.ID, header.Flags, header.Protocol)
				}

				if actual, loaded := routes.LoadOrStore(header.Src.String(), addr); loaded {
					if actual.(net.Addr).String() != addr.String() {
						log.Logf("[tun] %s <- %s: unexpected address mapping %s -> %s(actual %s)",
							tun.LocalAddr(), addr, header.Dst.String(), addr, actual.(net.Addr).String())
					}
				}

				if _, err := tun.Write(b[:n]); err != nil {
					return err
				}
				return nil
			}()

			if err != nil {
				errc <- err
				return
			}
		}
	}()

	err := <-errc
	if err != nil && err == io.EOF {
		err = nil
	}
	log.Logf("[tun] %s - %s: %v", tun.LocalAddr(), conn.LocalAddr(), err)
	return err
}

type tunConn struct {
	ifce *water.Interface
	addr net.Addr
}

func (c *tunConn) Read(b []byte) (n int, err error) {
	return c.ifce.Read(b)
}

func (c *tunConn) Write(b []byte) (n int, err error) {
	return c.ifce.Write(b)
}

func (c *tunConn) Close() (err error) {
	return c.ifce.Close()
}

func (c *tunConn) LocalAddr() net.Addr {
	return c.addr
}

func (c *tunConn) RemoteAddr() net.Addr {
	return &net.IPAddr{}
}

func (c *tunConn) SetDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *tunConn) SetReadDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *tunConn) SetWriteDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

type tunListener struct {
	conn             *net.UDPConn
	accepted, closed chan struct{}
}

// TunListener creates a listener for tun tunnel.
func TunListener(addr string) (Listener, error) {
	laddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, err
	}

	return &tunListener{
		conn:     conn,
		accepted: make(chan struct{}),
		closed:   make(chan struct{}),
	}, nil
}

func (l *tunListener) Accept() (net.Conn, error) {
	select {
	case <-l.accepted:
	default:
		close(l.accepted)
		return l.conn, nil
	}

	select {
	case <-l.closed:
	}

	return nil, errors.New("accept on closed listener")
}

func (l *tunListener) Addr() net.Addr {
	return l.conn.LocalAddr()
}

func (l *tunListener) Close() error {
	select {
	case <-l.closed:
		return errors.New("listener has been closed")
	default:
		close(l.closed)
	}
	return nil
}
