package guard

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ClientPool is exclusively for production forwarding. Request cancellation and
// deadlines belong to the host; probes use a separate per-sample transport.
type ClientPool struct {
	mu       sync.Mutex
	entries  map[string]*http.Transport
	lastUsed map[string]uint64
	sequence uint64
}

func newTransport(proxy string) (*http.Transport, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableCompression = true
	tr.MaxIdleConns = 256
	tr.MaxIdleConnsPerHost = 32
	tr.IdleConnTimeout = 90 * time.Second
	tr.ResponseHeaderTimeout = 0
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("代理地址无效")
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return nil, errors.New("代理端口无效")
			}
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return tr, nil
}

func (p *ClientPool) Client(proxy string) (*http.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = map[string]*http.Transport{}
	}
	if p.lastUsed == nil {
		p.lastUsed = map[string]uint64{}
	}
	tr := p.entries[proxy]
	if tr == nil {
		var err error
		tr, err = newTransport(proxy)
		if err != nil {
			return nil, err
		}
		if len(p.entries) >= 64 {
			var victim string
			oldest := ^uint64(0)
			for k := range p.entries {
				if p.lastUsed[k] < oldest {
					victim, oldest = k, p.lastUsed[k]
				}
			}
			p.entries[victim].CloseIdleConnections()
			delete(p.entries, victim)
			delete(p.lastUsed, victim)
		}
		p.entries[proxy] = tr
	}
	p.sequence++
	p.lastUsed[proxy] = p.sequence
	return &http.Client{Transport: tr, CheckRedirect: NoRedirect}, nil
}
func (p *ClientPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.entries {
		t.CloseIdleConnections()
	}
	p.entries = nil
	p.lastUsed = nil
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c > 127 || !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_"+string(rune(96))+"|~", c)) {
			return false
		}
	}
	return true
}
func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < 32 && s[i] != '\t') || s[i] == 127 {
			return false
		}
	}
	return true
}
