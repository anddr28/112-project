package main

import (
	"errors"
	"net"
	"sync"
	"time"
)

// tcpProxy — TCP-прокси клиент → go-core (TLS проходит насквозь), через который обрыв сети
// делается детерминированно:
//   - refuse: существующие соединения рвутся, listener закрывается (новые — connection refused),
//     как при пропаже маршрута/падении Wi-Fi с RST;
//   - blackhole: соединения не рвутся, но байты не идут в обе стороны, новые принимаются и
//     «висят» (как при потере пакетов); после восстановления поток продолжается.
type tcpProxy struct {
	target string
	addr   string

	mu    sync.Mutex
	ln    net.Listener
	up    bool
	mode  string
	gate  chan struct{} // закрыт — байты идут
	conns map[net.Conn]struct{}
	stats struct{ accepted, refusedDrops int }
}

func newProxy(target string) (*tcpProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	open := make(chan struct{})
	close(open)
	p := &tcpProxy{target: target, addr: ln.Addr().String(), ln: ln, up: true, gate: open, conns: map[net.Conn]struct{}{}}
	go p.acceptLoop(ln)
	return p, nil
}

func (p *tcpProxy) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *tcpProxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.up && p.mode == "refuse" {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *tcpProxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	c.Close()
}

func (p *tcpProxy) waitGate() {
	p.mu.Lock()
	g := p.gate
	p.mu.Unlock()
	<-g
}

func (p *tcpProxy) handle(c net.Conn) {
	if !p.track(c) {
		c.Close()
		return
	}
	defer p.untrack(c)
	p.waitGate()
	up, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	if !p.track(up) {
		up.Close()
		return
	}
	defer p.untrack(up)
	p.mu.Lock()
	p.stats.accepted++
	p.mu.Unlock()
	done := make(chan struct{}, 2)
	go func() { p.pipe(up, c); done <- struct{}{} }()
	go func() { p.pipe(c, up); done <- struct{}{} }()
	<-done
}

func (p *tcpProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			p.waitGate()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	dst.Close()
	src.Close()
}

// Cut обрывает «сеть» в выбранном режиме.
func (p *tcpProxy) Cut(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.up = false
	p.mode = mode
	switch mode {
	case "blackhole":
		p.gate = make(chan struct{})
	default: // refuse
		p.ln.Close()
		for c := range p.conns {
			c.Close()
			p.stats.refusedDrops++
		}
		p.conns = map[net.Conn]struct{}{}
	}
}

// Restore возвращает «сеть» (listener — на тот же адрес).
func (p *tcpProxy) Restore() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.up {
		return nil
	}
	if p.mode == "blackhole" {
		close(p.gate)
	} else {
		var ln net.Listener
		var err error
		for range 50 {
			if ln, err = net.Listen("tcp", p.addr); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			return errors.Join(errors.New("proxy: не удалось снова занять порт"), err)
		}
		p.ln = ln
		go p.acceptLoop(ln)
	}
	p.up = true
	return nil
}

func (p *tcpProxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ln.Close()
	for c := range p.conns {
		c.Close()
	}
}
