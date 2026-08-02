package app

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type Config struct {
	Origin        string
	RPID          string
	ListenAddress string
	DataDir       string
	MaxUploadSize int64
}

func ParseConfig(getenv func(string) string) (Config, error) {
	origin, rpid, err := parseOrigin(strings.TrimSpace(getenv("CAPSULE_ORIGIN")))
	if err != nil {
		return Config{}, err
	}

	listenAddress := strings.TrimSpace(getenv("CAPSULE_LISTEN_ADDR"))
	if listenAddress == "" {
		listenAddress = ":8080"
	}
	dataDir := strings.TrimSpace(getenv("CAPSULE_DATA_DIR"))
	if dataDir == "" {
		dataDir = "/data"
	}
	maxUploadSize := int64(100 << 20)
	if raw := strings.TrimSpace(getenv("CAPSULE_MAX_UPLOAD_MB")); raw != "" {
		megabytes, parseErr := strconv.ParseInt(raw, 10, 32)
		if parseErr != nil || megabytes < 1 || megabytes > 10240 {
			return Config{}, errors.New("CAPSULE_MAX_UPLOAD_MB must be an integer between 1 and 10240")
		}
		maxUploadSize = megabytes << 20
	}

	return Config{
		Origin:        origin,
		RPID:          rpid,
		ListenAddress: listenAddress,
		DataDir:       dataDir,
		MaxUploadSize: maxUploadSize,
	}, nil
}

func parseOrigin(raw string) (string, string, error) {
	if raw == "" {
		return "", "", errors.New("CAPSULE_ORIGIN is required, for example https://capsule.example.com")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse CAPSULE_ORIGIN: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("CAPSULE_ORIGIN must contain only a scheme and host, without credentials, path, query, or fragment")
	}
	hostname := parsed.Hostname()
	if hostname == "" || strings.ContainsAny(hostname, "\r\n") {
		return "", "", errors.New("CAPSULE_ORIGIN must contain a valid hostname")
	}
	loopback := strings.EqualFold(hostname, "localhost")
	if ip := net.ParseIP(hostname); ip != nil {
		loopback = ip.IsLoopback()
	}
	if !strings.EqualFold(parsed.Scheme, "https") && !(strings.EqualFold(parsed.Scheme, "http") && loopback) {
		return "", "", errors.New("CAPSULE_ORIGIN must use HTTPS except for localhost or a loopback address")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	hostname = strings.ToLower(hostname)
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return parsed.Scheme + "://" + host, hostname, nil
}
