package node

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"golang.org/x/net/proxy"
)

const (
	streamUnlockStatusRunning = "running"
	streamUnlockStatusSuccess = "success"
	streamUnlockStatusFailed  = "failed"

	streamUnlockResultUnlocked = "unlocked"
	streamUnlockResultBlocked  = "blocked"
	streamUnlockResultTimeout  = "timeout"
	streamUnlockResultError    = "error"
	streamUnlockResultUnknown  = "unknown"

	streamUnlockMaxConcurrency = 8
)

var (
	streamUnlockTaskMu   sync.Mutex
	streamUnlockTaskSeen = map[string]struct{}{}
)

type streamUnlockProxyContextKey struct{}

type streamProbe struct {
	StatusCode int
	FinalURL   string
	Body       string
	LatencyMs  int64
	Err        error
}

type streamServiceDefinition struct {
	Title          string
	URL            string
	SuccessMessage string
	BlockedPhrases []string
}

var genericStreamServices = map[string]streamServiceDefinition{
	"dazn": {
		Title:          "DAZN",
		URL:            "https://www.dazn.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your country", "not available in your region", "unsupported region"},
	},
	"bilibili": {
		Title:          "Bilibili",
		URL:            "https://www.bilibili.com/bangumi/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"region restricted", "copyright restricted", "not available"},
	},
	"bahamut": {
		Title:          "Bahamut Anime",
		URL:            "https://ani.gamer.com.tw/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"region restricted", "not available"},
	},
	"prime_video": {
		Title:          "Prime Video",
		URL:            "https://www.primevideo.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your location", "not available in your country"},
	},
	"hulu": {
		Title:          "Hulu",
		URL:            "https://www.hulu.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"available in the u.s.", "not available in your location"},
	},
	"hbo_max": {
		Title:          "Max",
		URL:            "https://www.max.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"peacock": {
		Title:          "Peacock",
		URL:            "https://www.peacocktv.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"paramount_plus": {
		Title:          "Paramount+",
		URL:            "https://www.paramountplus.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"discovery_plus": {
		Title:          "Discovery+",
		URL:            "https://www.discoveryplus.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"bbc_iplayer": {
		Title:          "BBC iPlayer",
		URL:            "https://www.bbc.co.uk/iplayer",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"only works in the uk", "not available in your country"},
	},
	"itvx": {
		Title:          "ITVX",
		URL:            "https://www.itv.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "only available in the uk"},
	},
	"channel4": {
		Title:          "Channel 4",
		URL:            "https://www.channel4.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "only available in the uk"},
	},
	"crunchyroll": {
		Title:          "Crunchyroll",
		URL:            "https://www.crunchyroll.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"apple_tv": {
		Title:          "Apple TV+",
		URL:            "https://tv.apple.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your country", "not available in your region"},
	},
	"spotify": {
		Title:          "Spotify",
		URL:            "https://www.spotify.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your country", "not available in your region"},
	},
	"deezer": {
		Title:          "Deezer",
		URL:            "https://www.deezer.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your country", "not available in your region"},
	},
	"tidal": {
		Title:          "TIDAL",
		URL:            "https://tidal.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your country", "not available in your region"},
	},
	"rakuten_viki": {
		Title:          "Rakuten Viki",
		URL:            "https://www.viki.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"u_next": {
		Title:          "U-NEXT",
		URL:            "https://video.unext.jp/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available", "outside japan"},
	},
	"niconico": {
		Title:          "Niconico",
		URL:            "https://www.nicovideo.jp/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "outside japan"},
	},
	"dmm": {
		Title:          "DMM TV",
		URL:            "https://tv.dmm.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "outside japan"},
	},
	"fod": {
		Title:          "FOD",
		URL:            "https://fod.fujitv.co.jp/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available", "outside japan"},
	},
	"viu": {
		Title:          "Viu",
		URL:            "https://www.viu.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"tvbanywhere": {
		Title:          "TVBAnywhere+",
		URL:            "https://www.tvbanywhere.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"mytv_super": {
		Title:          "myTV SUPER",
		URL:            "https://www.mytvsuper.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"kktv": {
		Title:          "KKTV",
		URL:            "https://www.kktv.me/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"line_tv": {
		Title:          "LINE TV",
		URL:            "https://www.linetv.tw/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"wavve": {
		Title:          "Wavve",
		URL:            "https://www.wavve.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"tving": {
		Title:          "TVING",
		URL:            "https://www.tving.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"watcha": {
		Title:          "WATCHA",
		URL:            "https://watcha.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"hotstar": {
		Title:          "Hotstar",
		URL:            "https://www.hotstar.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"jio_cinema": {
		Title:          "JioCinema",
		URL:            "https://www.jiocinema.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"sony_liv": {
		Title:          "SonyLIV",
		URL:            "https://www.sonyliv.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"zee5": {
		Title:          "ZEE5",
		URL:            "https://www.zee5.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"mewatch": {
		Title:          "meWATCH",
		URL:            "https://www.mewatch.sg/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"catchplay": {
		Title:          "Catchplay+",
		URL:            "https://www.catchplay.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"openai": {
		Title:          "OpenAI",
		URL:            "https://api.openai.com/v1/models",
		SuccessMessage: "api reachable",
		BlockedPhrases: []string{"unsupported_country_region_territory", "not available in your country"},
	},
	"claude": {
		Title:          "Claude",
		URL:            "https://claude.ai/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "unsupported country"},
	},
	"gemini": {
		Title:          "Gemini",
		URL:            "https://gemini.google.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your country", "isn't supported"},
	},
	"copilot": {
		Title:          "Copilot",
		URL:            "https://copilot.microsoft.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"perplexity": {
		Title:          "Perplexity",
		URL:            "https://www.perplexity.ai/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
	"poe": {
		Title:          "Poe",
		URL:            "https://poe.com/",
		SuccessMessage: "site reachable",
		BlockedPhrases: []string{"not available in your region", "not available in your country"},
	},
}

func (c *Controller) checkStreamUnlockTask(ctx context.Context) {
	task, err := c.apiClient.GetStreamUnlockTask(ctx)
	if err != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get stream unlock task failed")
		return
	}
	if task == nil || !task.Enabled || task.TaskID == "" {
		return
	}
	if !beginStreamUnlockTask(task.TaskID) {
		return
	}

	go c.runStreamUnlockTask(*task)
}

func beginStreamUnlockTask(taskID string) bool {
	streamUnlockTaskMu.Lock()
	defer streamUnlockTaskMu.Unlock()

	if _, ok := streamUnlockTaskSeen[taskID]; ok {
		return false
	}
	streamUnlockTaskSeen[taskID] = struct{}{}
	return true
}

func (c *Controller) runStreamUnlockTask(task panel.StreamUnlockTask) {
	started := time.Now()
	finalStatus := streamUnlockStatusFailed
	finalMessage := "stream unlock test aborted"
	var results []panel.StreamUnlockResult
	defer func() {
		if r := recover(); r != nil {
			finalStatus = streamUnlockStatusFailed
			finalMessage = fmt.Sprintf("stream unlock test panic: %v", r)
		}
		c.reportStreamUnlockStatus(task.TaskID, finalStatus, finalMessage, results, time.Since(started))
	}()

	services := normalizeStreamUnlockServices(task.Services)
	timeout := normalizeStreamUnlockTimeout(task.Timeout)
	if len(services) == 0 {
		finalMessage = "no supported services selected"
		return
	}

	proxyAddress, cleanup, err := c.openStreamUnlockProbeProxy()
	if err != nil {
		finalMessage = "start xray probe proxy failed: " + err.Error()
		return
	}
	defer cleanup()

	c.reportStreamUnlockStatus(task.TaskID, streamUnlockStatusRunning, "stream unlock test started", nil, 0)

	ctx, cancel := context.WithTimeout(context.Background(), streamUnlockTaskTimeout(timeout, len(services)))
	defer cancel()
	ctx = context.WithValue(ctx, streamUnlockProxyContextKey{}, proxyAddress)

	region := detectCloudflareRegion(ctx, timeout)
	results = runStreamUnlockChecks(ctx, services, region, timeout)
	if ctx.Err() != nil {
		finalMessage = "stream unlock test timed out"
		return
	}
	finalStatus = streamUnlockStatusSuccess
	finalMessage = "stream unlock test finished"
}

func streamUnlockTaskTimeout(timeout time.Duration, serviceCount int) time.Duration {
	if serviceCount < 1 {
		serviceCount = 1
	}
	batches := (serviceCount + streamUnlockMaxConcurrency - 1) / streamUnlockMaxConcurrency
	return timeout*time.Duration(batches+2) + 30*time.Second
}

func runStreamUnlockChecks(ctx context.Context, services []string, region string, timeout time.Duration) []panel.StreamUnlockResult {
	results := make([]panel.StreamUnlockResult, len(services))
	concurrency := streamUnlockMaxConcurrency
	if len(services) < concurrency {
		concurrency = len(services)
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, service := range services {
		i, service := i, service
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = streamUnlockTimeoutResult(service, region, 0)
				return
			}
			results[i] = runStreamUnlockCheck(ctx, service, region, timeout)
		}()
	}
	wg.Wait()

	for i, service := range services {
		if results[i].Service == "" {
			results[i] = streamUnlockTimeoutResult(service, region, 0)
		}
	}
	return results
}

func streamUnlockTimeoutResult(service string, region string, latencyMs int64) panel.StreamUnlockResult {
	return streamUnlockResult(service, streamUnlockServiceTitle(service), streamUnlockResultTimeout, region, "test timed out", latencyMs)
}

func streamUnlockServiceTitle(service string) string {
	switch service {
	case "tls_rtt":
		return "TLS RTT"
	case "https_latency":
		return "HTTPS Latency"
	case "netflix":
		return "Netflix"
	case "disney_plus":
		return "Disney+"
	case "youtube":
		return "YouTube Premium"
	case "tiktok":
		return "TikTok"
	case "abema":
		return "Abema"
	case "steam":
		return "Steam"
	case "openai":
		return "OpenAI"
	default:
		if definition, ok := genericStreamServices[service]; ok {
			return definition.Title
		}
		return service
	}
}

func (c *Controller) reportStreamUnlockStatus(taskID string, status string, message string, results []panel.StreamUnlockResult, duration time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.apiClient.ReportStreamUnlockStatus(ctx, panel.StreamUnlockReport{
		TaskID:     taskID,
		Status:     status,
		Message:    message,
		Results:    results,
		DurationMs: duration.Milliseconds(),
	}); err != nil {
		log.WithFields(log.Fields{
			"tag":    c.tag,
			"task":   taskID,
			"status": status,
			"err":    err,
		}).Error("Report stream unlock status failed")
	}
}

func (c *Controller) openStreamUnlockProbeProxy() (string, func(), error) {
	if c.server == nil || c.tag == "" {
		return "", func() {}, fmt.Errorf("xray core is not ready")
	}

	port, err := allocateLocalTCPPort()
	if err != nil {
		return "", func() {}, err
	}
	if err := c.server.AddStreamUnlockProbeInbound(c.tag, port); err != nil {
		_ = c.server.RemoveStreamUnlockProbeInbound(c.tag)
		if retryErr := c.server.AddStreamUnlockProbeInbound(c.tag, port); retryErr != nil {
			return "", func() {}, retryErr
		}
	}

	cleanup := func() {
		if err := c.server.RemoveStreamUnlockProbeInbound(c.tag); err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Warn("Remove stream unlock probe inbound failed")
		}
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), cleanup, nil
}

func allocateLocalTCPPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || addr.Port <= 0 {
		return 0, fmt.Errorf("invalid local tcp listener")
	}
	return addr.Port, nil
}

func normalizeStreamUnlockServices(services []string) []string {
	if len(services) == 0 {
		services = []string{"tls_rtt", "https_latency", "youtube", "netflix", "disney_plus", "bilibili", "tiktok", "dazn", "abema", "bahamut", "openai", "steam"}
	}

	seen := make(map[string]struct{}, len(services))
	result := make([]string, 0, len(services))
	for _, service := range services {
		key := strings.ToLower(strings.TrimSpace(service))
		switch key {
		case "disney+", "disneyplus":
			key = "disney_plus"
		case "disney":
			key = "disney_plus"
		case "max":
			key = "hbo_max"
		case "paramount+":
			key = "paramount_plus"
		case "discovery+":
			key = "discovery_plus"
		case "unext":
			key = "u_next"
		case "tvb":
			key = "tvbanywhere"
		case "apple", "appletv", "apple_tv_plus":
			key = "apple_tv"
		case "viki":
			key = "rakuten_viki"
		case "jiocinema", "jio":
			key = "jio_cinema"
		case "sonyliv":
			key = "sony_liv"
		case "linetv":
			key = "line_tv"
		case "mytv", "mytvsuper":
			key = "mytv_super"
		}
		switch key {
		case "tls_rtt", "https_latency", "netflix", "disney_plus", "youtube", "tiktok", "steam", "abema":
		default:
			if _, ok := genericStreamServices[key]; !ok {
				continue
			}
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

func normalizeStreamUnlockTimeout(seconds int) time.Duration {
	if seconds < 5 {
		seconds = 5
	}
	if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func runStreamUnlockCheck(ctx context.Context, service string, region string, timeout time.Duration) panel.StreamUnlockResult {
	switch service {
	case "tls_rtt":
		return checkTLSRTT(ctx, region, timeout)
	case "https_latency":
		return checkHTTPSLatency(ctx, region, timeout)
	case "netflix":
		return checkNetflixUnlock(ctx, region, timeout)
	case "disney_plus":
		return checkDisneyUnlock(ctx, region, timeout)
	case "youtube":
		return checkYouTubeUnlock(ctx, region, timeout)
	case "tiktok":
		return checkTikTokUnlock(ctx, region, timeout)
	case "abema":
		return checkAbemaUnlock(ctx, region, timeout)
	case "steam":
		return checkSteamUnlock(ctx, region, timeout)
	case "openai":
		return checkOpenAIUnlock(ctx, region, timeout)
	default:
		if definition, ok := genericStreamServices[service]; ok {
			return checkGenericStreamService(ctx, service, definition, region, timeout)
		}
		return streamUnlockResult(service, service, streamUnlockResultUnknown, region, "unsupported service", 0)
	}
}

func checkTLSRTT(ctx context.Context, region string, timeout time.Duration) panel.StreamUnlockResult {
	started := time.Now()
	rawConn, err := streamDialContext(ctx, "tcp", "www.cloudflare.com:443", timeout)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return probeErrorResult("tls_rtt", "TLS RTT", region, streamProbe{LatencyMs: latency, Err: err})
	}
	defer rawConn.Close()

	tlsConn := tls.Client(rawConn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: "www.cloudflare.com",
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return probeErrorResult("tls_rtt", "TLS RTT", region, streamProbe{LatencyMs: time.Since(started).Milliseconds(), Err: err})
	}
	latency = time.Since(started).Milliseconds()
	return streamUnlockResult("tls_rtt", "TLS RTT", streamUnlockResultUnlocked, region, "tls handshake ok", latency)
}

func checkHTTPSLatency(ctx context.Context, region string, timeout time.Duration) panel.StreamUnlockResult {
	probe := streamHTTPProbe(ctx, "https://www.gstatic.com/generate_204", timeout, 1024)
	if probe.Err != nil {
		return probeErrorResult("https_latency", "HTTPS Latency", region, probe)
	}
	if probe.StatusCode >= 200 && probe.StatusCode < 400 {
		return streamUnlockResult("https_latency", "HTTPS Latency", streamUnlockResultUnlocked, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
	}
	return streamUnlockResult("https_latency", "HTTPS Latency", streamUnlockResultUnknown, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
}

func checkNetflixUnlock(ctx context.Context, region string, timeout time.Duration) panel.StreamUnlockResult {
	probe := streamHTTPProbe(ctx, "https://www.netflix.com/title/80018499", timeout, 128*1024)
	if probe.Err != nil {
		return probeErrorResult("netflix", "Netflix", region, probe)
	}

	body := strings.ToLower(probe.Body)
	if probe.StatusCode == http.StatusForbidden || probe.StatusCode == http.StatusUnavailableForLegalReasons {
		return streamUnlockResult("netflix", "Netflix", streamUnlockResultBlocked, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
	}
	if strings.Contains(body, "not available in your country") || strings.Contains(body, "unavailable in your region") {
		return streamUnlockResult("netflix", "Netflix", streamUnlockResultBlocked, region, "content unavailable in this region", probe.LatencyMs)
	}
	if probe.StatusCode >= 200 && probe.StatusCode < 400 {
		return streamUnlockResult("netflix", "Netflix", streamUnlockResultUnlocked, region, "title page reachable", probe.LatencyMs)
	}
	return streamUnlockResult("netflix", "Netflix", streamUnlockResultUnknown, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
}

func checkDisneyUnlock(ctx context.Context, region string, timeout time.Duration) panel.StreamUnlockResult {
	probe := streamHTTPProbe(ctx, "https://www.disneyplus.com/", timeout, 128*1024)
	if probe.Err != nil {
		return probeErrorResult("disney_plus", "Disney+", region, probe)
	}

	body := strings.ToLower(probe.Body)
	if probe.StatusCode == http.StatusForbidden || probe.StatusCode == http.StatusUnavailableForLegalReasons {
		return streamUnlockResult("disney_plus", "Disney+", streamUnlockResultBlocked, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
	}
	if strings.Contains(body, "not available in your region") || strings.Contains(body, "not available in your country") {
		return streamUnlockResult("disney_plus", "Disney+", streamUnlockResultBlocked, region, "service unavailable in this region", probe.LatencyMs)
	}
	if probe.StatusCode >= 200 && probe.StatusCode < 400 {
		return streamUnlockResult("disney_plus", "Disney+", streamUnlockResultUnlocked, region, "site reachable", probe.LatencyMs)
	}
	return streamUnlockResult("disney_plus", "Disney+", streamUnlockResultUnknown, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
}

func checkYouTubeUnlock(ctx context.Context, region string, timeout time.Duration) panel.StreamUnlockResult {
	probe := streamHTTPProbe(ctx, "https://www.youtube.com/premium", timeout, 192*1024)
	if probe.Err != nil {
		return probeErrorResult("youtube", "YouTube Premium", region, probe)
	}

	body := strings.ToLower(probe.Body)
	if found := regexp.MustCompile(`(?i)"countryCode"\s*:\s*"([A-Z]{2})"`).FindStringSubmatch(probe.Body); len(found) == 2 {
		region = found[1]
	} else if found := regexp.MustCompile(`(?i)"GL"\s*:\s*"([A-Z]{2})"`).FindStringSubmatch(probe.Body); len(found) == 2 {
		region = found[1]
	}
	if strings.Contains(body, "premium is not available in your country") || strings.Contains(body, "not available in your country") {
		return streamUnlockResult("youtube", "YouTube Premium", streamUnlockResultBlocked, region, "premium unavailable in this country", probe.LatencyMs)
	}
	if probe.StatusCode >= 200 && probe.StatusCode < 400 {
		return streamUnlockResult("youtube", "YouTube Premium", streamUnlockResultUnlocked, region, "premium page reachable", probe.LatencyMs)
	}
	return streamUnlockResult("youtube", "YouTube Premium", streamUnlockResultUnknown, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
}

func checkTikTokUnlock(ctx context.Context, region string, timeout time.Duration) panel.StreamUnlockResult {
	probe := streamHTTPProbe(ctx, "https://www.tiktok.com/", timeout, 128*1024)
	if probe.Err != nil {
		return probeErrorResult("tiktok", "TikTok", region, probe)
	}
	if probe.StatusCode == http.StatusForbidden || probe.StatusCode == http.StatusUnavailableForLegalReasons {
		return streamUnlockResult("tiktok", "TikTok", streamUnlockResultBlocked, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
	}
	if probe.StatusCode >= 200 && probe.StatusCode < 400 {
		return streamUnlockResult("tiktok", "TikTok", streamUnlockResultUnlocked, region, "site reachable", probe.LatencyMs)
	}
	return streamUnlockResult("tiktok", "TikTok", streamUnlockResultUnknown, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
}

func checkAbemaUnlock(ctx context.Context, region string, timeout time.Duration) panel.StreamUnlockResult {
	probe := streamHTTPProbe(ctx, "https://api.abema.io/v1/ip/check?device=pc", timeout, 32*1024)
	if probe.Err != nil {
		return probeErrorResult("abema", "Abema", region, probe)
	}

	if found := regexp.MustCompile(`(?i)"isoCountryCode"\s*:\s*"([A-Z]{2})"`).FindStringSubmatch(probe.Body); len(found) == 2 {
		region = found[1]
	}
	if region == "JP" && probe.StatusCode >= 200 && probe.StatusCode < 400 {
		return streamUnlockResult("abema", "Abema", streamUnlockResultUnlocked, region, "Japan endpoint accepted", probe.LatencyMs)
	}
	if probe.StatusCode == http.StatusForbidden || probe.StatusCode == http.StatusUnavailableForLegalReasons {
		return streamUnlockResult("abema", "Abema", streamUnlockResultBlocked, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
	}
	if probe.StatusCode >= 200 && probe.StatusCode < 400 {
		return streamUnlockResult("abema", "Abema", streamUnlockResultBlocked, region, "not in JP region", probe.LatencyMs)
	}
	return streamUnlockResult("abema", "Abema", streamUnlockResultUnknown, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
}

func checkSteamUnlock(ctx context.Context, region string, timeout time.Duration) panel.StreamUnlockResult {
	probe := streamHTTPProbe(ctx, "https://store.steampowered.com/api/appdetails?appids=292030&filters=price_overview", timeout, 64*1024)
	if probe.Err != nil {
		return probeErrorResult("steam", "Steam", region, probe)
	}

	if found := regexp.MustCompile(`(?i)"currency"\s*:\s*"([A-Z]{3})"`).FindStringSubmatch(probe.Body); len(found) == 2 {
		return streamUnlockResult("steam", "Steam", streamUnlockResultUnlocked, region, "currency: "+found[1], probe.LatencyMs)
	}
	if probe.StatusCode >= 200 && probe.StatusCode < 400 {
		return streamUnlockResult("steam", "Steam", streamUnlockResultUnlocked, region, "store reachable", probe.LatencyMs)
	}
	return streamUnlockResult("steam", "Steam", streamUnlockResultUnknown, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
}

func checkOpenAIUnlock(ctx context.Context, region string, timeout time.Duration) panel.StreamUnlockResult {
	probe := streamHTTPProbe(ctx, "https://api.openai.com/v1/models", timeout, 64*1024)
	if probe.Err != nil {
		return probeErrorResult("openai", "OpenAI", region, probe)
	}

	body := strings.ToLower(probe.Body)
	if probe.StatusCode == http.StatusUnauthorized {
		return streamUnlockResult("openai", "OpenAI", streamUnlockResultUnlocked, region, "API reachable", probe.LatencyMs)
	}
	if probe.StatusCode == http.StatusForbidden || strings.Contains(body, "unsupported_country_region_territory") {
		return streamUnlockResult("openai", "OpenAI", streamUnlockResultBlocked, region, "region blocked", probe.LatencyMs)
	}
	if probe.StatusCode >= 200 && probe.StatusCode < 500 {
		return streamUnlockResult("openai", "OpenAI", streamUnlockResultUnlocked, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
	}
	return streamUnlockResult("openai", "OpenAI", streamUnlockResultUnknown, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
}

func checkGenericStreamService(ctx context.Context, service string, definition streamServiceDefinition, region string, timeout time.Duration) panel.StreamUnlockResult {
	probe := streamHTTPProbe(ctx, definition.URL, timeout, 128*1024)
	if probe.Err != nil {
		return probeErrorResult(service, definition.Title, region, probe)
	}

	body := strings.ToLower(probe.Body)
	if probe.StatusCode == http.StatusForbidden || probe.StatusCode == http.StatusUnavailableForLegalReasons {
		return streamUnlockResult(service, definition.Title, streamUnlockResultBlocked, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
	}
	for _, phrase := range definition.BlockedPhrases {
		if phrase != "" && strings.Contains(body, strings.ToLower(phrase)) {
			return streamUnlockResult(service, definition.Title, streamUnlockResultBlocked, region, phrase, probe.LatencyMs)
		}
	}
	if probe.StatusCode >= 200 && probe.StatusCode < 400 {
		return streamUnlockResult(service, definition.Title, streamUnlockResultUnlocked, region, definition.SuccessMessage, probe.LatencyMs)
	}
	if probe.StatusCode == http.StatusUnauthorized {
		return streamUnlockResult(service, definition.Title, streamUnlockResultUnlocked, region, "auth required but reachable", probe.LatencyMs)
	}
	return streamUnlockResult(service, definition.Title, streamUnlockResultUnknown, region, fmt.Sprintf("HTTP %d", probe.StatusCode), probe.LatencyMs)
}

func detectCloudflareRegion(ctx context.Context, timeout time.Duration) string {
	probe := streamHTTPProbe(ctx, "https://www.cloudflare.com/cdn-cgi/trace", timeout, 4096)
	if probe.Err != nil || probe.StatusCode < 200 || probe.StatusCode >= 400 {
		return ""
	}
	for _, line := range strings.Split(probe.Body, "\n") {
		if strings.HasPrefix(line, "loc=") {
			return strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(line, "loc=")))
		}
	}
	return ""
}

func streamHTTPProbe(ctx context.Context, targetURL string, timeout time.Duration, maxBody int64) streamProbe {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return streamProbe{Err: err}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				return streamDialContext(ctx, network, addr, timeout)
			},
			ForceAttemptHTTP2:     true,
			ResponseHeaderTimeout: timeout,
			TLSHandshakeTimeout:   timeout,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 6 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return streamProbe{LatencyMs: latency, Err: err}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if readErr != nil {
		return streamProbe{StatusCode: resp.StatusCode, FinalURL: resp.Request.URL.String(), LatencyMs: latency, Err: readErr}
	}
	return streamProbe{
		StatusCode: resp.StatusCode,
		FinalURL:   resp.Request.URL.String(),
		Body:       string(body),
		LatencyMs:  latency,
	}
}

func streamDialContext(ctx context.Context, network string, addr string, timeout time.Duration) (net.Conn, error) {
	if proxyAddress := streamUnlockProbeProxyAddress(ctx); proxyAddress != "" {
		dialer, err := proxy.SOCKS5("tcp", proxyAddress, nil, proxy.Direct)
		if err != nil {
			return nil, err
		}
		if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
			return contextDialer.DialContext(ctx, network, addr)
		}
		return dialer.Dial(network, addr)
	}

	dialer := &net.Dialer{Timeout: timeout}
	return dialer.DialContext(ctx, network, addr)
}

func streamUnlockProbeProxyAddress(ctx context.Context) string {
	proxyAddress, _ := ctx.Value(streamUnlockProxyContextKey{}).(string)
	return strings.TrimSpace(proxyAddress)
}

func probeErrorResult(service string, title string, region string, probe streamProbe) panel.StreamUnlockResult {
	status := streamUnlockResultError
	if isTimeoutError(probe.Err) {
		status = streamUnlockResultTimeout
	}
	return streamUnlockResult(service, title, status, region, truncateStreamUnlockMessage(probe.Err.Error(), 180), probe.LatencyMs)
}

func streamUnlockResult(service string, title string, status string, region string, message string, latencyMs int64) panel.StreamUnlockResult {
	return panel.StreamUnlockResult{
		Service:   service,
		Title:     title,
		Status:    status,
		Region:    strings.ToUpper(strings.TrimSpace(region)),
		Message:   truncateStreamUnlockMessage(message, 180),
		LatencyMs: latencyMs,
	}
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func truncateStreamUnlockMessage(message string, limit int) string {
	message = strings.TrimSpace(message)
	if len(message) <= limit {
		return message
	}
	return message[:limit]
}
