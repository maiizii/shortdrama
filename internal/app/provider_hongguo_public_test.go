package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const hongguoPublicFixtureURL = "https://media.example.org/plain/episode.mp4"

func hongguoPublicFixtureDownloader(t *testing.T, transport rankingTransport) *Downloader {
	t.Helper()
	d := rankingTestDownloader(t, transport)
	d.cfg.Retries = 1
	return d
}

func hongguoPublicFixtureResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		Request:    request,
	}
}

func TestHongguoWebPublicMediaUsesViewerCookieAndReturnsPlainURL(t *testing.T) {
	var homeCalls, playbackCalls atomic.Int32
	d := hongguoPublicFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/":
			homeCalls.Add(1)
			response := hongguoPublicFixtureResponse(request, http.StatusOK, "<html></html>")
			response.Header.Set("Set-Cookie", "ttwid=fixture%7Ctoken; Path=/; HttpOnly; Secure")
			return response, nil
		case "/api/video/protected/fallback-play-info":
			playbackCalls.Add(1)
			if request.Method != http.MethodPost {
				t.Errorf("public playback must use POST, got %s", request.Method)
			}
			if request.Header.Get("Origin") != hongguoBaseURL || !strings.Contains(request.Header.Get("Referer"), "/player/7000000000000000001/7000000000000000002") {
				t.Errorf("public playback lost its source headers: origin=%q referer=%q", request.Header.Get("Origin"), request.Header.Get("Referer"))
			}
			if request.Header.Get("Cookie") != "ttwid=fixture%7Ctoken" {
				t.Errorf("public playback must reuse the viewer cookie, got %q", request.Header.Get("Cookie"))
			}
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("public playback body is not JSON: %v", err)
			}
			if body["item_id"] != "7000000000000000002" {
				t.Errorf("public playback sent the wrong item: %q", body["item_id"])
			}
			return hongguoPublicFixtureResponse(request, http.StatusOK,
				`{"main_url":"`+hongguoPublicFixtureURL+`","url_expire":1900000000,"base_resp":{"StatusCode":0,"StatusMessage":""}}`), nil
		default:
			t.Errorf("unexpected resource: %s", request.URL.Path)
			return nil, errors.New("only the homepage and the public playback fixture are allowed")
		}
	})
	media, err := d.resolveHongguoWebPublicMedia(context.Background(), "7000000000000000001", "7000000000000000002")
	if err != nil {
		t.Fatalf("public playback failed: %v", err)
	}
	if media.URL != hongguoPublicFixtureURL || media.Referer != hongguoBaseURL+"/" {
		t.Fatalf("unexpected media: %+v", media)
	}
	if homeCalls.Load() != 1 || playbackCalls.Load() != 1 {
		t.Fatalf("unexpected request counts: home=%d playback=%d", homeCalls.Load(), playbackCalls.Load())
	}
	// 第二轮必须复用 Cookie，不再请求首页。
	if _, err := d.resolveHongguoWebPublicMedia(context.Background(), "7000000000000000001", "7000000000000000002"); err != nil {
		t.Fatalf("second public playback failed: %v", err)
	}
	if homeCalls.Load() != 1 {
		t.Fatalf("viewer cookie was not cached: home calls=%d", homeCalls.Load())
	}
}

func TestHongguoWebPublicMediaReportsRestrictedEpisodes(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "restricted", status: http.StatusForbidden, body: `{"error":"public_fallback_unavailable"}`, want: "HTTP 403"},
		{name: "missing url", status: http.StatusOK, body: `{"main_url":"","base_resp":{"StatusCode":0}}`, want: "没有返回可播放地址"},
		{name: "upstream failure", status: http.StatusOK, body: `{"main_url":"` + hongguoPublicFixtureURL + `","base_resp":{"StatusCode":1}}`, want: "暂不可用"},
		{name: "malformed", status: http.StatusOK, body: `not-json`, want: "返回格式异常"},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := hongguoPublicFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/" {
					response := hongguoPublicFixtureResponse(request, http.StatusOK, "<html></html>")
					response.Header.Set("Set-Cookie", "ttwid=fixture; Path=/")
					return response, nil
				}
				return hongguoPublicFixtureResponse(request, test.status, test.body), nil
			})
			_, err := d.resolveHongguoWebPublicMedia(context.Background(), "7000000000000000001", "7000000000000000002")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q in the error, got %v", test.want, err)
			}
		})
	}
}

func TestHongguoPublicMediaFallsBackAfterAppAndWebPagesFail(t *testing.T) {
	d := hongguoPublicFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Host == "api5-normal-sinfonlineb.fqnovel.com":
			return hongguoPublicFixtureResponse(request, http.StatusOK, ""), nil
		case request.URL.Path == "/":
			response := hongguoPublicFixtureResponse(request, http.StatusOK, "<html></html>")
			response.Header.Set("Set-Cookie", "ttwid=fixture; Path=/")
			return response, nil
		case request.URL.Path == "/api/video/protected/fallback-play-info":
			return hongguoPublicFixtureResponse(request, http.StatusOK,
				`{"main_url":"`+hongguoPublicFixtureURL+`","base_resp":{"StatusCode":0}}`), nil
		case strings.HasPrefix(request.URL.Path, "/player/"):
			return hongguoPublicFixtureResponse(request, http.StatusOK,
				`<html><script>window._ROUTER_DATA = {"loaderData":{"player_page":{"vid":"7000000000000000002","series_id":"7000000000000000001","protectedPlayback":true}}}</script></html>`), nil
		default:
			t.Errorf("unexpected resource: %s", request.URL.String())
			return nil, errors.New("unexpected fixture request")
		}
	})
	task := Task{DramaID: "hongguo:7000000000000000001", Chapter: Chapter{Source: sourceHongguo, VideoURL: "hongguo-cenc://7000000000000000002"}}
	media, err := d.resolveHongguoMedia(context.Background(), task)
	if err != nil {
		t.Fatalf("expected the public playback leg to succeed, got %v", err)
	}
	if media.URL != hongguoPublicFixtureURL {
		t.Fatalf("unexpected media URL: %q", media.URL)
	}
}

func TestHongguoViewerCookieExpires(t *testing.T) {
	var homeCalls atomic.Int32
	d := hongguoPublicFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		homeCalls.Add(1)
		response := hongguoPublicFixtureResponse(request, http.StatusOK, "<html></html>")
		response.Header.Set("Set-Cookie", "ttwid=fixture; Path=/; Max-Age=60")
		return response, nil
	})
	if _, err := d.hongguoWebCookie(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.hongguoCookieMu.Lock()
	d.hongguoCookieExpires = time.Now().Add(-time.Second)
	d.hongguoCookieMu.Unlock()
	if _, err := d.hongguoWebCookie(context.Background()); err != nil {
		t.Fatal(err)
	}
	if homeCalls.Load() != 2 {
		t.Fatalf("expired viewer cookie was not refreshed: home calls=%d", homeCalls.Load())
	}
}

func TestHongguoWebPublicMediaRetriesRateLimitedAttempt(t *testing.T) {
	var homeCalls, playbackCalls atomic.Int32
	d := hongguoPublicFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/" {
			homeCalls.Add(1)
			response := hongguoPublicFixtureResponse(request, http.StatusOK, "<html></html>")
			response.Header.Set("Set-Cookie", "ttwid=fixture-"+strconv.Itoa(int(homeCalls.Load()))+"; Path=/")
			return response, nil
		}
		if playbackCalls.Add(1) == 1 {
			return hongguoPublicFixtureResponse(request, http.StatusForbidden, `{"error":"public_fallback_unavailable"}`), nil
		}
		if request.Header.Get("Cookie") != "ttwid=fixture-2" {
			t.Errorf("retry must use a fresh viewer cookie, got %q", request.Header.Get("Cookie"))
		}
		return hongguoPublicFixtureResponse(request, http.StatusOK,
			`{"main_url":"`+hongguoPublicFixtureURL+`","base_resp":{"StatusCode":0}}`), nil
	})
	media, err := d.resolveHongguoWebPublicMedia(context.Background(), "7000000000000000001", "7000000000000000002")
	if err != nil {
		t.Fatalf("rate limited attempt was not retried: %v", err)
	}
	if media.URL != hongguoPublicFixtureURL || playbackCalls.Load() != 2 || homeCalls.Load() != 2 {
		t.Fatalf("unexpected retry result: media=%+v playback=%d home=%d", media, playbackCalls.Load(), homeCalls.Load())
	}
}
