package ipp

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const socketHost = "localhost:631"

// cupsSocketPath 只读取部署配置，不接受来自 HTTP 请求的 socket 路径。
func cupsSocketPath() (string, error) {
	socketPath := strings.TrimSpace(os.Getenv("CUPS_SOCKET_PATH"))
	if socketPath != "" && (!filepath.IsAbs(socketPath) || strings.ContainsRune(socketPath, '\x00')) {
		return "", fmt.Errorf("CUPS_SOCKET_PATH must be an absolute filesystem path")
	}
	return socketPath, nil
}

// ValidateCUPSConfig 在启动时检查配置语法；socket 可以由外部 cupsd 稍后创建。
func ValidateCUPSConfig() error {
	_, err := cupsSocketPath()
	return err
}

// validSocketRequestURI 限制固定 socket 上可访问的资源，避免将用户输入发到
// CUPS 管理端点。主机只作为 URI 占位符，不参与连接或 TCP 的 SSRF 策略。
func validSocketRequestURI(u *url.URL) bool {
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || path.Clean(u.Path) != u.Path {
		return false
	}
	if u.Path == "/" || u.Path == "/printers" {
		return true
	}
	parts := strings.Split(u.Path, "/")
	return len(parts) == 3 && (parts[1] == "printers" || parts[1] == "classes") && parts[2] != ""
}

// printerRequestURI 保留队列路径，将 IPP URI 转为 HTTP 传输 URI。Socket 模式
// 统一使用 localhost，兼容 CUPS 自报主机名以及旧打印记录中的 TCP 地址。
func printerRequestURI(raw string) (string, error) {
	if err := validatePrinterURI(raw); err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	socketPath, err := cupsSocketPath()
	if err != nil {
		return "", err
	}
	if socketPath != "" {
		u.Scheme, u.Host = "http", socketHost
	} else {
		scheme := strings.ToLower(u.Scheme)
		// RFC 8010：IPP/IPPS 转为 HTTP/HTTPS 时，省略的端口必须补为 631。
		if (scheme == "ipp" || scheme == "ipps") && u.Port() == "" {
			u.Host = net.JoinHostPort(u.Hostname(), "631")
		}
		switch scheme {
		case "ipp", "http":
			u.Scheme = "http"
		case "ipps", "https":
			u.Scheme = "https"
		}
	}
	return u.String(), nil
}
