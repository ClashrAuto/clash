package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"io"
	"strings"
	"time"

	"github.com/ClashrAuto/clash/common/atomic"
	"github.com/ClashrAuto/clash/common/queue"
	"github.com/ClashrAuto/clash/common/utils"
	"github.com/ClashrAuto/clash/common/xsync"
	"github.com/ClashrAuto/clash/component/ca"
	C "github.com/ClashrAuto/clash/constant"
	"github.com/ClashrAuto/clash/log"
	"github.com/VividCortex/ewma"
)

var UnifiedDelay = atomic.NewBool(false)

const (
	defaultHistoriesNum = 10
)

type internalProxyState struct {
	alive   atomic.Bool
	history *queue.Queue[C.DelayHistory]
}

type Proxy struct {
	C.ProxyAdapter
	alive   atomic.Bool
	history *queue.Queue[C.DelayHistory]
	extra   xsync.Map[string, *internalProxyState]
}

// Adapter implements C.Proxy
func (p *Proxy) Adapter() C.ProxyAdapter {
	return p.ProxyAdapter
}

// AliveForTestUrl implements C.Proxy
func (p *Proxy) AliveForTestUrl(url string) bool {
	if state, ok := p.extra.Load(url); ok {
		return state.alive.Load()
	}

	return p.alive.Load()
}

// DialContext implements C.ProxyAdapter
func (p *Proxy) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	conn, err := p.ProxyAdapter.DialContext(ctx, metadata)
	return conn, err
}

// ListenPacketContext implements C.ProxyAdapter
func (p *Proxy) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	pc, err := p.ProxyAdapter.ListenPacketContext(ctx, metadata)
	return pc, err
}

// DelayHistory implements C.Proxy
func (p *Proxy) DelayHistory() []C.DelayHistory {
	queueM := p.history.Copy()
	histories := []C.DelayHistory{}
	for _, item := range queueM {
		histories = append(histories, item)
	}
	return histories
}

// DelayHistoryForTestUrl implements C.Proxy
func (p *Proxy) DelayHistoryForTestUrl(url string) []C.DelayHistory {
	var queueM []C.DelayHistory

	if state, ok := p.extra.Load(url); ok {
		queueM = state.history.Copy()
	}
	histories := []C.DelayHistory{}
	for _, item := range queueM {
		histories = append(histories, item)
	}
	return histories
}

// ExtraDelayHistories return all delay histories for each test URL
// implements C.Proxy
func (p *Proxy) ExtraDelayHistories() map[string]C.ProxyState {
	histories := map[string]C.ProxyState{}

	p.extra.Range(func(k string, v *internalProxyState) bool {
		testUrl := k
		state := v

		queueM := state.history.Copy()
		var history []C.DelayHistory

		for _, item := range queueM {
			history = append(history, item)
		}

		histories[testUrl] = C.ProxyState{
			Alive:   state.alive.Load(),
			History: history,
		}
		return true
	})
	return histories
}

// LastDelayForTestUrl return last history record of the specified URL. if proxy is not alive, return the max value of uint16.
// implements C.Proxy
func (p *Proxy) LastDelayForTestUrl(url string) (delay uint16) {
	var maxDelay uint16 = 0xffff

	alive := false
	var history C.DelayHistory

	if state, ok := p.extra.Load(url); ok {
		alive = state.alive.Load()
		history = state.history.Last()
	}

	if !alive || history.Delay == 0 {
		return maxDelay
	}
	return history.Delay
}

// MarshalJSON implements C.ProxyAdapter
func (p *Proxy) MarshalJSON() ([]byte, error) {
	inner, err := p.ProxyAdapter.MarshalJSON()
	if err != nil {
		return inner, err
	}

	mapping := map[string]any{}
	_ = json.Unmarshal(inner, &mapping)
	mapping["history"] = p.DelayHistory()
	mapping["extra"] = p.ExtraDelayHistories()
	mapping["alive"] = p.alive.Load()
	mapping["name"] = p.Name()
	mapping["udp"] = p.SupportUDP()
	mapping["uot"] = p.SupportUOT()

	proxyInfo := p.ProxyInfo()
	mapping["xudp"] = proxyInfo.XUDP
	mapping["tfo"] = proxyInfo.TFO
	mapping["mptcp"] = proxyInfo.MPTCP
	mapping["smux"] = proxyInfo.SMUX
	mapping["interface"] = proxyInfo.Interface
	mapping["dialer-proxy"] = proxyInfo.DialerProxy
	mapping["routing-mark"] = proxyInfo.RoutingMark

	return json.Marshal(mapping)
}

// URLTest get the delay for the specified URL
// implements C.Proxy
func (p *Proxy) URLTest(ctx context.Context, url string, expectedStatus utils.IntRanges[uint16]) (t uint16, err error) {
	var satisfied bool

	defer func() {
		alive := err == nil
		record := C.DelayHistory{Time: time.Now()}
		if alive {
			record.Delay = t
		}

		p.alive.Store(alive)
		p.history.Put(record)
		if p.history.Len() > defaultHistoriesNum {
			p.history.Pop()
		}

		state, ok := p.extra.Load(url)
		if !ok {
			state = &internalProxyState{
				history: queue.New[C.DelayHistory](defaultHistoriesNum),
				alive:   atomic.NewBool(true),
			}
			p.extra.Store(url, state)
		}

		if !satisfied {
			record.Delay = 0
			alive = false
		}

		state.alive.Store(alive)
		state.history.Put(record)
		if state.history.Len() > defaultHistoriesNum {
			state.history.Pop()
		}

	}()

	unifiedDelay := UnifiedDelay.Load()

	addr, err := urlToMetadata(url)
	if err != nil {
		return
	}

	start := time.Now()
	instance, err := p.DialContext(ctx, &addr)
	if err != nil {
		return
	}
	defer func() {
		_ = instance.Close()
	}()

	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return
	}
	req = req.WithContext(ctx)

	tlsConfig, err := ca.GetTLSConfig(ca.Option{})
	if err != nil {
		return
	}

	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return instance, nil
		},
		// from http.DefaultTransport
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsConfig,
	}

	client := http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	defer client.CloseIdleConnections()

	resp, err := client.Do(req)

	if err != nil {
		return
	}

	_ = resp.Body.Close()

	if unifiedDelay {
		second := time.Now()
		var ignoredErr error
		var secondResp *http.Response
		secondResp, ignoredErr = client.Do(req)
		if ignoredErr == nil {
			resp = secondResp
			_ = resp.Body.Close()
			start = second
		} else {
			if strings.HasPrefix(url, "http://") {
				log.Errorln("%s failed to get the second response from %s: %v", p.Name(), url, ignoredErr)
				log.Warnln("It is recommended to use HTTPS for provider.health-check.url and group.url to ensure better reliability. Due to some proxy providers hijacking test addresses and not being compatible with repeated HEAD requests, using HTTP may result in failed tests.")
			}
		}
	}

	satisfied = resp != nil && (expectedStatus == nil || expectedStatus.Check(uint16(resp.StatusCode)))
	t = uint16(time.Since(start) / time.Millisecond)
	return
}

func NewProxy(adapter C.ProxyAdapter) *Proxy {
	return &Proxy{
		ProxyAdapter: adapter,
		history:      queue.New[C.DelayHistory](defaultHistoriesNum),
		alive:        atomic.NewBool(true),
	}
}

func urlToMetadata(rawURL string) (addr C.Metadata, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}

	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			err = fmt.Errorf("%s scheme not Support", rawURL)
			return
		}
	}

	err = addr.SetRemoteAddress(net.JoinHostPort(u.Hostname(), port))
	return
}

// LastDelay returns the last recorded delay; if not alive or no delay, returns max uint16.
func (p *Proxy) LastDelay() (delay uint16) {
	var max uint16 = 0xffff
	if !p.alive.Load() {
		return max
	}
	history := p.history.Last()
	if history.Delay == 0 {
		return max
	}
	return history.Delay
}

// LastSpeed returns the last recorded download speed; 0 when not available or not alive.
func (p *Proxy) LastSpeed() (speed float64) {
	if !p.alive.Load() {
		return 0
	}
	history := p.history.Last()
	if history.Speed == 0 {
		return 0
	}
	return history.Speed
}

// URLDownload performs a timed HTTP GET through this proxy and returns estimated download speed.
func (p *Proxy) URLDownload(timeout int, url string) (t float64, err error) {
	defer func() {
		p.alive.Store(err == nil)
		record := C.DelayHistory{Time: time.Now()}
		if err == nil {
			record.Speed = t
			record.Delay = p.LastDelay()
		}
		p.history.Put(record)
		if p.history.Len() > defaultHistoriesNum {
			p.history.Pop()
		}
	}()

	addr, err := urlToMetadata(url)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*time.Duration(timeout))
	defer cancel()

	instance, err := p.DialContext(ctx, &addr)
	if err != nil {
		return
	}
	defer func() { _ = instance.Close() }()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req = req.WithContext(ctx)

	transport := &http.Transport{
		Dial: func(string, string) (net.Conn, error) {
			return instance, nil
		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		t = 0
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		downloadTestTime := time.Millisecond * time.Duration(timeout)
		timeStart := time.Now()
		timeEnd := timeStart.Add(downloadTestTime)

		contentLength := resp.ContentLength
		if contentLength <= 0 {
			contentLength = 1 << 20 // fallback buffer when unknown length
		}
		buffer := make([]byte, contentLength)

		var contentRead int64 = 0
		timeSlice := downloadTestTime / 100
		timeCounter := 1
		var lastContentRead int64 = 0
		nextTime := timeStart.Add(timeSlice * time.Duration(timeCounter))
		e := ewma.NewMovingAverage()

		for {
			currentTime := time.Now()
			if currentTime.After(nextTime) {
				timeCounter++
				nextTime = timeStart.Add(timeSlice * time.Duration(timeCounter))
				e.Add(float64(contentRead - lastContentRead))
				lastContentRead = contentRead
			}
			if currentTime.After(timeEnd) {
				break
			}
			n, rerr := resp.Body.Read(buffer)
			contentRead += int64(n)
			if rerr != nil {
				if rerr != io.EOF {
					break
				}
				// finalize EWMA with remaining proportion
				e.Add(float64(contentRead-lastContentRead) / (float64(nextTime.Sub(currentTime)) / float64(timeSlice)))
				break
			}
		}
		// average bytes per slice; convert to bytes per second
		t = e.Value() / (downloadTestTime.Seconds() / 100)
	} else {
		t = 0
	}
	return
}
