package app

import (
	"context"
	"errors"
	"example.com/oci-pull-proxy/internal/config"
	"example.com/oci-pull-proxy/internal/registry"
	"example.com/oci-pull-proxy/internal/server"
	"example.com/oci-pull-proxy/internal/tlsmanager"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var Version = "dev"

func Main(args []string, out, errout io.Writer) int {
	if len(args) > 1 || (len(args) == 1 && args[0] != "check" && args[0] != "version") {
		fmt.Fprintln(errout, "usage: oci-pull-proxy [check|version]")
		return 2
	}
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(out, "oci-pull-proxy", Version)
		return 0
	}
	logf := log.New(out, "", log.LstdFlags).Printf
	if len(args) == 0 {
		logf("正在检查配置")
	}
	c, e := config.Load()
	if e != nil {
		fmt.Fprintf(errout, "配置错误：%v\n", e)
		return 1
	}
	if len(args) == 1 {
		if _, e = config.ResolveToken(c.Token, ".mirror-token"); e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		if e = Check(c); e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		fmt.Fprintln(out, "配置和目录权限检查通过；未申请证书。")
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e = runConfigured(ctx, c, logf); e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	return 0
}
func Check(c config.Config) error {
	if e := c.Validate(); e != nil {
		return fmt.Errorf("配置错误：%w", e)
	}
	if e := config.CheckDirectory(c.Cache.Directory); e != nil {
		return fmt.Errorf("目录权限错误：%w", e)
	}
	if c.Server.TLSMode == "cloudflare" {
		return nil
	}
	if c.Server.TLSMode == "auto" {
		if e := config.CheckDirectory(c.ACME.Directory); e != nil {
			return fmt.Errorf("目录权限错误：%w", e)
		}
		return nil
	}
	_, e := tlsmanager.New(c)
	if e != nil {
		return fmt.Errorf("证书加载失败：%w", e)
	}
	return nil
}
func Run(ctx context.Context, c config.Config) error {
	return run(ctx, c, log.Printf)
}
func run(ctx context.Context, c config.Config, logf func(string, ...any)) error {
	logf("正在检查配置")
	return runConfigured(ctx, c, logf)
}
func runConfigured(ctx context.Context, c config.Config, logf func(string, ...any)) error {
	var tokenErr error
	c.Token, tokenErr = config.ResolveToken(c.Token, ".mirror-token")
	if tokenErr != nil {
		return tokenErr
	}
	if e := Check(c); e != nil {
		return e
	}
	logf("配置检查通过")
	if c.Server.TLSMode == "cloudflare" {
		p, err := registry.New(c)
		if err != nil {
			return err
		}
		h := server.Cloudflare(c.Server.Domain, server.Handler(p, func() bool { return true }))
		return runHTTP(ctx, c, h, logf)
	}
	m, e := tlsmanager.New(c, logf)
	if e != nil {
		return e
	}
	p, e := registry.New(c)
	if e != nil {
		return e
	}
	https := server.New(c.Server.Listen, server.Handler(p, m.Ready))
	https.TLSConfig = m.TLSConfig()
	secure, e := net.Listen("tcp", https.Addr)
	if e != nil {
		return fmt.Errorf("端口监听失败：HTTPS_LISTEN %s: %w；请检查端口占用和监听地址", https.Addr, e)
	}
	defer secure.Close()
	plain := server.New(c.Server.HTTPListen, m.HTTPHandler(server.Redirect(c.Server.Domain)))
	httpListener, e := net.Listen("tcp", plain.Addr)
	if e != nil {
		return fmt.Errorf("端口监听失败：HTTP_LISTEN %s: %w；请检查端口占用和监听地址", plain.Addr, e)
	}
	defer httpListener.Close()
	logf("服务端口已监听：HTTPS %s，HTTP %s", secure.Addr(), httpListener.Addr())
	m.Startup(ctx)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- https.ServeTLS(secure, "", "") }()
	go func() { errs <- plain.Serve(httpListener) }()
	go m.Run(runCtx)
	select {
	case <-ctx.Done():
	case e = <-errs:
		if errors.Is(e, http.ErrServerClosed) {
			e = nil
		}
	}
	cancel()
	shutdown, done := context.WithTimeout(context.Background(), 20*time.Second)
	defer done()
	// Shut down both listeners together so new plaintext requests cannot prolong exit.
	stopped := make(chan struct{})
	go func() {
		if plain.Shutdown(shutdown) != nil {
			plain.Close()
		}
		close(stopped)
	}()
	if https.Shutdown(shutdown) != nil {
		https.Close()
	}
	<-stopped
	return e
}

// runHTTP owns the HTTP-only origin listener and drains requests on shutdown.
func runHTTP(ctx context.Context, c config.Config, h http.Handler, logf func(string, ...any)) error {
	plain := server.New(c.Server.HTTPListen, h)
	// Route OPTIONS * through the same source gate as all other requests.
	plain.DisableGeneralOptionsHandler = true
	listener, err := net.Listen("tcp", plain.Addr)
	if err != nil {
		return fmt.Errorf("端口监听失败：HTTP_LISTEN %s: %w；请检查端口占用和监听地址", plain.Addr, err)
	}
	defer listener.Close()
	errs := make(chan error, 1)
	go func() { errs <- plain.Serve(listener) }()
	logf("HTTP 回源服务已就绪，仅接受 Cloudflare 来源：%s", listener.Addr())
	logf("外部访问地址：https://%s", c.Server.Domain)
	logf("docker login %s -u %s", c.Server.Domain, c.Auth.Username)
	logf("docker pull %s/alpine:latest", c.Server.Domain)
	select {
	case <-ctx.Done():
	case err = <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if plain.Shutdown(shutdown) != nil {
		plain.Close()
	}
	return err
}
