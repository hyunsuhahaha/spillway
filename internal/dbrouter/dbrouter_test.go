package dbrouter

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// greeter accepts connections and writes its name, then echoes.
func greeter(t *testing.T, name string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.Write([]byte(name + "\n"))
				buf := make([]byte, 64)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write(buf[:n])
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func startRouter(t *testing.T, target string) (*Router, string) {
	t.Helper()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	r := New(Config{Listen: addr, AdminListen: "127.0.0.1:0", Target: target, HoldTimeout: 2 * time.Second, DialTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Run(ctx)
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return r, addr
}

func greeting(t *testing.T, addr string) (string, net.Conn) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	return strings.TrimSpace(line), c
}

func admin(t *testing.T, r *Router, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.AdminHandler().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	return rec
}

func TestSwitchTargetKillsOldConnections(t *testing.T) {
	a, b := greeter(t, "A"), greeter(t, "B")
	r, addr := startRouter(t, a)

	name, old := greeting(t, addr)
	if name != "A" {
		t.Fatalf("got %q want A", name)
	}
	admin(t, r, "PUT", "/target", `{"addr":"`+b+`"}`)

	// The old connection must be closed so clients reconnect to the new primary.
	old.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := old.Read(make([]byte, 1)); err == nil {
		t.Fatalf("old connection still open after switch")
	}
	name, c := greeting(t, addr)
	c.Close()
	if name != "B" {
		t.Fatalf("got %q want B", name)
	}
	if st := r.Status(); st.Target != b || st.Switches != 1 {
		t.Fatalf("status %+v", st)
	}
}

func TestPauseHoldsNewConnectionsUntilResume(t *testing.T) {
	a := greeter(t, "A")
	r, addr := startRouter(t, a)
	admin(t, r, "POST", "/pause", `{}`)
	go func() {
		time.Sleep(300 * time.Millisecond)
		admin(t, r, "POST", "/resume", "")
	}()
	start := time.Now()
	name, c := greeting(t, addr)
	c.Close()
	if name != "A" || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("connection was not held (name %q after %v)", name, time.Since(start))
	}
}
