package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const hongguoBaseURL = "https://hongguoduanju.com"

// hongguoWebUserAgent 是网页端播放页使用的桌面浏览器标识。公开播放接口会按 UA
// 区分访问者，用移动端 UA 更容易被判定为不可播。
const hongguoWebUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

func (d *Downloader) fetchHongguoDramas(ctx context.Context) ([]Drama, error) {
	initialized := hongguoCatalogInitialized(d.hongguoCatalogSnapshot())
	dramas, appErr := d.fetchHongguoAppCatalog(ctx)
	if len(dramas) > 0 || appErr == nil && initialized {
		return dramas, appErr
	}
	if ctx.Err() != nil {
		return dramas, ctx.Err()
	}
	webDramas, webErr := d.fetchHongguoWebDramas(ctx)
	if len(webDramas) > 0 && webErr == nil {
		return webDramas, nil
	}
	return webDramas, errors.Join(appErr, webErr)
}

func (d *Downloader) fetchHongguoWebDramas(ctx context.Context) ([]Drama, error) {
	routes := []struct {
		path string
		name string
	}{
		{path: "real-drama", name: "真人剧"},
		{path: "comic-drama", name: "漫剧"},
		{path: "ai-drama", name: "AI剧"},
		{path: "comic", name: "动漫"},
	}
	type routeResult struct {
		dramas []Drama
		err    error
	}
	results := make(chan routeResult, len(routes))
	for _, route := range routes {
		route := route
		go func() {
			dramas, err := d.fetchHongguoRoute(ctx, route.path, route.name)
			results <- routeResult{dramas: dramas, err: err}
		}()
	}
	seen := map[string]bool{}
	var out []Drama
	var failures []error
	for range routes {
		result := <-results
		if result.err != nil {
			failures = append(failures, result.err)
		}
		for _, drama := range result.dramas {
			if drama.ID != "" && !seen[drama.ID] {
				seen[drama.ID] = true
				out = append(out, drama)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DisplayTitle() < out[j].DisplayTitle() })
	return out, errors.Join(failures...)
}

func (d *Downloader) fetchHongguoRoute(ctx context.Context, route, category string) ([]Drama, error) {
	pages := d.cfg.MaxPagesPerSort
	if pages < 1 {
		pages = 1
	}
	var dramas []Drama
	seen := map[string]bool{}
	for page := 1; page <= pages; page++ {
		items, totalPages, err := d.fetchHongguoCategoryPage(ctx, route+"?page="+strconv.Itoa(page), category)
		if err != nil {
			return dramas, fmt.Errorf("%s: %w", category, err)
		}
		if totalPages > pages {
			pages = totalPages
			if pages > 500 {
				pages = 500
			}
		}
		reportLibraryProgress(ctx, sourceHongguo, items, nil, false)
		added := 0
		for _, drama := range items {
			if !seen[drama.ID] {
				seen[drama.ID] = true
				dramas = append(dramas, drama)
				added++
			}
		}
		if added == 0 || len(items) < 24 {
			break
		}
	}
	return dramas, nil
}

func (d *Downloader) fetchHongguoCategory(ctx context.Context, route, category string) ([]Drama, error) {
	items, _, err := d.fetchHongguoCategoryPage(ctx, route, category)
	return items, err
}

func (d *Downloader) fetchHongguoCategoryPage(ctx context.Context, route, category string) ([]Drama, int, error) {
	body, err := d.fetchProviderText(ctx, hongguoBaseURL+"/category/"+route, hongguoBaseURL+"/")
	if err != nil {
		return nil, 0, err
	}
	data := parseRouterData(body)
	page := routerLoaderMap(data, "category_page", "category_$")
	if len(page) == 0 || page["isSuccess"] == false {
		return nil, 0, errors.New("红果分类数据不可用，可能是页面结构或访问权限变化")
	}
	items := anyList(page["recommendList"])
	out := make([]Drama, 0, len(items))
	for _, item := range items {
		if dr := hongguoDramaFromAny(item, category); dr.ID != "" {
			out = append(out, dr)
		}
	}
	pages, _ := strconv.Atoi(mapString(nestedMap(page, "pagination"), "totalPages"))
	return out, pages, nil
}

func (d *Downloader) fetchHongguoChapters(ctx context.Context, sourceID string) (string, []Chapter, error) {
	sourceID = strings.TrimPrefix(strings.TrimSpace(sourceID), "hg-series-v1:")
	if sourceID == "" {
		return "", nil, fmt.Errorf("empty hongguo sourceID")
	}
	entry, appErr := d.hongguoAppDetail(ctx, sourceID)
	if appErr == nil {
		return entry.Drama.DisplayTitle(), append([]Chapter(nil), entry.Chapters...), nil
	}
	if ctx.Err() != nil {
		return "", nil, ctx.Err()
	}
	title, chapters, webErr := d.fetchHongguoWebChapters(ctx, sourceID)
	if webErr == nil {
		return title, chapters, nil
	}
	return "", nil, fmt.Errorf("App 详情失败: %v；网页详情失败: %w", publicError(appErr), webErr)
}

func (d *Downloader) fetchHongguoWebChapters(ctx context.Context, sourceID string) (string, []Chapter, error) {
	body, err := d.fetchProviderText(ctx, hongguoBaseURL+"/detail?series_id="+url.QueryEscape(sourceID), hongguoBaseURL+"/")
	if err != nil {
		return "", nil, err
	}
	data := parseRouterData(body)
	page := routerLoaderMap(data, "detail_page", "detail_")
	detail, _ := page["seriesDetail"].(map[string]any)
	if len(detail) == 0 {
		return "", nil, fmt.Errorf("hongguo detail is empty")
	}
	title := firstNonEmpty(mapString(detail, "series_name", "series_title", "name"), sourceID)
	vids := anyList(detail["vid_list"])
	chapters := make([]Chapter, 0, len(vids))
	for i, v := range vids {
		vid := strings.TrimSpace(fmt.Sprint(v))
		if vid == "" || vid == "<nil>" {
			continue
		}
		idx := i + 1
		chapters = append(chapters, Chapter{ID: providerChapterID(sourceHongguo, sourceID, vid), Source: sourceHongguo, Title: fmt.Sprintf("第%d集", idx), VideoURL: "hongguo-cenc://" + vid, CurrentEpisode: rawEpisode(idx)})
	}
	if len(chapters) == 0 {
		return title, nil, errors.New("红果详情没有返回剧集 ID")
	}
	return title, chapters, nil
}

func parseRouterData(raw string) map[string]any {
	idx := regexp.MustCompile(`(?s)(?:window\.)?_ROUTER_DATA\s*=\s*`).FindStringIndex(raw)
	if idx == nil {
		return nil
	}
	var data map[string]any
	dec := json.NewDecoder(strings.NewReader(raw[idx[1]:]))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return nil
	}
	return data
}

func (d *Downloader) resolveHongguoMedia(ctx context.Context, task Task) (providerMedia, error) {
	source, seriesID, ok := splitProviderDramaID(task.DramaID)
	if !ok || source != sourceHongguo {
		return providerMedia{}, fmt.Errorf("红果章节缺少剧 ID，请刷新剧库后重新加入队列")
	}
	videoID := strings.TrimPrefix(task.Chapter.VideoURL, "hongguo-cenc://")
	if !hongguoNumericID.MatchString(videoID) || !hongguoNumericID.MatchString(seriesID) {
		return providerMedia{}, fmt.Errorf("红果章节 ID 无效，请重新获取章节")
	}
	media, nativeErr := d.resolveHongguoAppMedia(ctx, videoID)
	if nativeErr == nil {
		return media, nil
	}
	if err := ctx.Err(); err != nil {
		return providerMedia{}, err
	}
	media, pageErr := d.resolveHongguoWebMedia(ctx, seriesID, videoID)
	if pageErr == nil {
		return media, nil
	}
	if err := ctx.Err(); err != nil {
		return providerMedia{}, err
	}
	media, pagePublicErr := d.resolveHongguoWebPublicMedia(ctx, seriesID, videoID)
	if pagePublicErr == nil {
		return media, nil
	}
	if err := ctx.Err(); err != nil {
		return providerMedia{}, err
	}
	media, apiErr := d.resolveHongguoPlaybackAPI(ctx, seriesID, videoID)
	if apiErr == nil {
		return media, nil
	}
	return providerMedia{}, fmt.Errorf("红果 App 取流失败：%v；网页取流失败：%v；公开播放接口失败：%v；备用取流失败：%w", publicError(nativeErr), publicError(pageErr), publicError(pagePublicErr), apiErr)
}

func (d *Downloader) resolveHongguoWebMedia(ctx context.Context, seriesID, videoID string) (providerMedia, error) {
	pageURL := hongguoBaseURL + "/player/" + url.PathEscape(seriesID) + "/" + url.PathEscape(videoID)
	body, err := d.fetchProviderText(ctx, pageURL, hongguoBaseURL+"/")
	if err != nil {
		return providerMedia{}, err
	}
	page := routerLoaderMap(parseRouterData(body), "player_", "player_page")
	if mapString(page, "vid") != videoID || mapString(page, "series_id") != seriesID {
		return providerMedia{}, fmt.Errorf("红果未返回所请求的剧集，可能仅允许网页试看；请在站点确认该集的访问权限")
	}
	info, _ := page["video_player_info"].(map[string]any)
	mediaURL := mapString(info, "main_url")
	if !isProviderHTTPMediaURL(mediaURL) {
		return providerMedia{}, fmt.Errorf("红果该集未提供公开播放地址，可能需要登录或 App 授权；不会将试看集冒充该集下载")
	}
	duration, _ := strconv.ParseFloat(mapString(info, "duration"), 64)
	return providerMedia{URL: mediaURL, Referer: d.providerBaseURL(sourceHongguo) + "/", Duration: time.Duration(duration * float64(time.Second))}, nil
}

// resolveHongguoWebPublicMedia 走网页端的公开播放接口。2026-10 起播放页不再下发
// video_player_info，改为受保护播放（DASH + CENC），但网页仍保留一条明文 MP4 直链：
// POST /api/video/protected/fallback-play-info。该接口要求带 ttwid Cookie，否则对
// 大部分分集返回 401/403，所以先访问站点首页取 Cookie，再请求接口。上游对同一来源
// 的短时间重复请求会临时判 403，此时换一个新 Cookie 并稍等重试即可恢复。
func (d *Downloader) resolveHongguoWebPublicMedia(ctx context.Context, seriesID, videoID string) (providerMedia, error) {
	payload, err := json.Marshal(map[string]string{"item_id": videoID})
	if err != nil {
		return providerMedia{}, err
	}
	endpoint := hongguoBaseURL + "/api/video/protected/fallback-play-info?aid=8662"
	referer := hongguoBaseURL + "/player/" + url.PathEscape(seriesID) + "/" + url.PathEscape(videoID)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 1200 * time.Millisecond):
			case <-ctx.Done():
				return providerMedia{}, ctx.Err()
			}
			d.invalidateHongguoWebCookie()
		}
		cookie, err := d.hongguoWebCookie(ctx)
		if err != nil {
			return providerMedia{}, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return providerMedia{}, err
		}
		// 该接口用 401/403 表示「这一集对外不可播」，不是反爬封锁；标记后限流器
		// 不会因此暂停整个站点，其他分集仍可继续取流。
		request = request.WithContext(context.WithValue(request.Context(), providerRejectionKey{}, true))
		request.Header.Set("User-Agent", hongguoWebUserAgent)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", hongguoBaseURL)
		request.Header.Set("Referer", referer)
		if cookie != "" {
			request.Header.Set("Cookie", cookie)
		}
		media, retry, err := d.hongguoPublicMediaResponse(request)
		if err == nil {
			return media, nil
		}
		lastErr = err
		if !retry || ctx.Err() != nil {
			return providerMedia{}, err
		}
	}
	return providerMedia{}, lastErr
}

// hongguoPublicMediaResponse 解析一次公开播放请求。retry 表示上游只是临时拒绝
// （401/403/429），换 Cookie 或稍后重试有机会恢复。
func (d *Downloader) hongguoPublicMediaResponse(request *http.Request) (providerMedia, bool, error) {
	response, err := d.doCatalogRequestWithTimeout(request, providerTimeout)
	if err != nil {
		return providerMedia{}, false, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
	if err != nil {
		return providerMedia{}, false, err
	}
	if len(content) > providerMaxBodyBytes {
		return providerMedia{}, false, fmt.Errorf("红果公开播放接口返回内容过大")
	}
	if response.StatusCode != http.StatusOK {
		retry := response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests
		return providerMedia{}, retry, fmt.Errorf("红果公开播放接口 HTTP %d：%s", response.StatusCode, hongguoPublicError(content))
	}
	var result struct {
		MainURL   string `json:"main_url"`
		URLExpire int64  `json:"url_expire"`
		BaseResp  struct {
			StatusCode int `json:"StatusCode"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(content, &result); err != nil {
		return providerMedia{}, false, fmt.Errorf("红果公开播放接口返回格式异常")
	}
	if result.BaseResp.StatusCode != 0 {
		return providerMedia{}, false, fmt.Errorf("红果公开播放接口暂不可用（%d）", result.BaseResp.StatusCode)
	}
	mediaURL := strings.TrimSpace(result.MainURL)
	if !isProviderHTTPMediaURL(mediaURL) {
		return providerMedia{}, false, fmt.Errorf("红果公开播放接口没有返回可播放地址，该集可能仍受限")
	}
	return providerMedia{URL: mediaURL, Referer: d.providerBaseURL(sourceHongguo) + "/"}, false, nil
}

func (d *Downloader) invalidateHongguoWebCookie() {
	d.hongguoCookieMu.Lock()
	d.hongguoCookie, d.hongguoCookieExpires = "", time.Time{}
	d.hongguoCookieMu.Unlock()
}

func hongguoPublicError(content []byte) string {
	message := strings.TrimSpace(string(content))
	if len(message) > 160 {
		message = message[:160]
	}
	if message == "" {
		return "空响应"
	}
	return message
}

// hongguoWebCookie 取站点首页下发的 Cookie（主要为 ttwid）。该值只需在进程内短期复用，
// 过期后由下一次取流自动重新获取。
func (d *Downloader) hongguoWebCookie(ctx context.Context) (string, error) {
	d.hongguoCookieMu.Lock()
	cached := d.hongguoCookie
	expires := d.hongguoCookieExpires
	d.hongguoCookieMu.Unlock()
	if cached != "" && time.Now().Before(expires) {
		return cached, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, hongguoBaseURL+"/", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", hongguoWebUserAgent)
	response, err := d.doCatalogRequestWithTimeout(request, providerTimeout)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	var values []string
	for _, cookie := range response.Cookies() {
		if cookie.Name == "" || len(cookie.Value) > 512 {
			continue
		}
		values = append(values, cookie.Name+"="+cookie.Value)
	}
	joined := strings.Join(values, "; ")
	if joined == "" {
		return "", nil
	}
	d.hongguoCookieMu.Lock()
	d.hongguoCookie, d.hongguoCookieExpires = joined, time.Now().Add(30*time.Minute)
	d.hongguoCookieMu.Unlock()
	return joined, nil
}

func hongguoCoverAddress(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 8192 {
			continue
		}
		parsed, err := url.Parse(value)
		if err == nil && validImageURL(parsed) {
			return value
		}
	}
	return ""
}

func hongguoDramaFromAny(v any, category string) Drama {
	m, ok := v.(map[string]any)
	if !ok {
		return Drama{}
	}
	vd, _ := m["video_data"].(map[string]any)
	if len(vd) == 0 {
		vd = m
	}
	sourceID := firstNonEmpty(mapString(vd, "series_id_str", "series_id"), mapString(m, "series_id_str", "series_id"), mapString(vd, "keyword"), mapString(m, "keyword"))
	if !hongguoNumericID.MatchString(sourceID) {
		return Drama{}
	}
	title := firstNonEmpty(mapString(vd, "series_title", "series_name", "title"), mapString(m, "series_name", "name"), sourceID)
	cover := firstNonEmpty(mapString(vd, "series_cover", "cover"), mapString(m, "series_cover"))
	intro := firstNonEmpty(mapString(vd, "series_intro", "video_desc"), mapString(m, "series_intro"))
	count := firstNonEmpty(mapString(vd, "episode_cnt"), mapString(m, "episode_cnt"))
	remark := firstNonEmpty(mapString(vd, "episode_right_text"), mapString(m, "episode_right_text"))
	releaseStatus := releaseStatusFromRemark(remark)
	if mapString(vd, "series_status") == "1" {
		releaseStatus = "finished"
	} else if mapString(vd, "series_status") == "0" {
		releaseStatus = "ongoing"
	}
	if remark == "" && count != "" {
		remark = "共" + count + "集"
	}
	tags := mapStringSlice(vd, "tags")
	for _, value := range anyList(vd["category_list"]) {
		item, _ := value.(map[string]any)
		if name := mapString(item, "name"); name != "" && !slices.Contains(tags, name) {
			tags = append(tags, name)
		}
	}
	var categories []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(mapString(vd, "category_schema")), &categories) == nil {
		for _, item := range categories {
			if item.Name != "" && !slices.Contains(tags, item.Name) {
				tags = append(tags, item.Name)
			}
		}
	}
	genre := mapString(vd, "category_name", "categoryName", "category")
	if genre == "" && len(tags) > 0 {
		genre = tags[0]
	}
	return Drama{ID: providerDramaID(sourceHongguo, sourceID), Source: sourceHongguo, SourceID: sourceID, Title: title, Name: title, Desc: intro, Intro: intro, Cover: cover, CoverURL: cover, CategoryName: firstNonEmpty(genre, category), ChannelName: "红果", Remark: remark, TotalEpisode: count, EpisodeCount: count, Tags: tags, ReleaseStatus: releaseStatus, Score: mapString(vd, "score"), Views: mapString(vd, "series_play_cnt", "play_cnt"), Heat: hongguoHeat(vd), OnlineDate: hongguoOnlineDate(vd, time.Now())}
}

func nestedMap(v any, keys ...string) map[string]any {
	cur, _ := v.(map[string]any)
	for _, key := range keys {
		if cur == nil {
			return nil
		}
		cur, _ = cur[key].(map[string]any)
	}
	return cur
}
func routerLoaderMap(data map[string]any, names ...string) map[string]any {
	loader, _ := data["loaderData"].(map[string]any)
	for _, name := range names {
		if page, _ := loader[name].(map[string]any); len(page) > 0 {
			return page
		}
	}
	for key, value := range loader {
		for _, name := range names {
			if strings.TrimSuffix(name, "$") != "" && strings.HasPrefix(key, strings.TrimSuffix(name, "$")) {
				if page, _ := value.(map[string]any); len(page) > 0 {
					return page
				}
			}
		}
	}
	return nil
}

func anyList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		for _, key := range []string{"list", "items", "data"} {
			if out := anyList(x[key]); len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for key, values := range v {
		out[key] = append([]string(nil), values...)
	}
	return out
}
