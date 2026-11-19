package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"unicode"

	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

const pastedPageSource = "pasted-page"
const pageCandidateLimit = 12
const pageFormatLimit = 16

func validPageCandidateID(id string) bool {
	if len(id) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func stalePageCandidate() *model.Inspection {
	return &model.Inspection{Formats: []model.Format{}, Reason: "网页上的所选视频或格式已变化，请重新预览网页"}
}

func inspectPageCandidates(req *base.Request, extra *model.ReqExtra, parent *videoInfo, cfg model.Config) (*videoInfo, *model.Inspection, error) {
	unsupported := &model.Inspection{Formats: []model.Format{}, Reason: "playlist or multi-video media is unsupported"}
	if extra.PageCandidateID != "" {
		unsupported = stalePageCandidate()
	}
	pageURL := validReferer(parent.WebpageURL)
	pageOrigin, _ := headerOrigin(pageURL)
	requestOrigin, _ := headerOrigin(req.URL)
	if parent.Type != "playlist" || parent.ExtractorKey != "Generic" || pageOrigin == "" || pageOrigin != requestOrigin || !availablePageVideo(parent) || !publicPageContext(extra) {
		return parent, unsupported, nil
	}
	var entries []videoInfo
	if json.Unmarshal(parent.Entries, &entries) != nil || len(entries) < 2 || len(entries) > pageCandidateLimit {
		return parent, unsupported, nil
	}
	candidates := make([]model.PageCandidate, 0, len(entries))
	occurrences := make(map[string]int, len(entries))
	var selected *videoInfo
	for i := range entries {
		entry := &entries[i]
		if !preparePageVideo(entry, pageURL, pageOrigin) {
			return parent, unsupported, nil
		}
		ids := make([]string, 0, len(entry.Formats))
		for _, format := range entry.Formats {
			ids = append(ids, format.FormatID)
		}
		sort.Strings(ids)
		key := strings.Join(ids, ":")
		// Two players can point to identical files. Distinguish occurrences only
		// within that identical set, so moving other players cannot change identity.
		occurrences[key]++
		id := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d", pageURL, key, occurrences[key]))))
		entry.ID = id
		candidates = append(candidates, model.PageCandidate{ID: id, Title: entry.Title, Thumbnail: entry.Thumbnail, Duration: entry.Duration})
		if id == extra.PageCandidateID {
			selected = entry
		}
	}
	interpreter, err := mediaBridgeInterpreter(cfg)
	if err != nil || interpreter == "" {
		unsupported.Reason = "网页多视频下载需要支持来源隔离的 Python yt-dlp"
		return parent, unsupported, nil
	}
	if extra.PageCandidateID == "" {
		return parent, &model.Inspection{Title: parent.Title, Formats: []model.Format{}, Candidates: candidates, Reason: "请选择网页中的一个视频"}, nil
	}
	if selected == nil {
		return parent, unsupported, nil
	}
	result := inspectionFromInfo(selected)
	for i, format := range selected.Formats {
		result.Formats[i].Label = format.FormatNote
	}
	if extra.Format != "" {
		var ok bool
		selected, ok = selectedInfo(selected, result, extra.Format)
		if !ok {
			return parent, unsupported, nil
		}
	}
	return selected, result, nil
}

func publicPageContext(extra *model.ReqExtra) bool {
	if len(extra.Cookies) != 0 || len(extra.ScopedHeaders) != 0 {
		return false
	}
	for key := range extra.Header {
		if !strings.EqualFold(key, "Referer") && !strings.EqualFold(key, "User-Agent") {
			return false
		}
	}
	return true
}

func availablePageVideo(info *videoInfo) bool {
	return !info.HasDRM && !info.IsLive && (info.LiveStatus == "" || info.LiveStatus == "not_live" || info.LiveStatus == "was_live")
}

func preparePageVideo(info *videoInfo, pageURL, pageOrigin string) bool {
	if (info.Type != "" && info.Type != "video") || info.ExtractorKey != "HTML5MediaEmbed" || validReferer(info.WebpageURL) != pageURL ||
		(len(info.Entries) != 0 && string(info.Entries) != "null") || !availablePageVideo(info) ||
		info.ManifestURL != "" || len(info.RequestedFormats) != 0 || info.VCodec == "none" ||
		len(info.Formats) == 0 || len(info.Formats) > pageFormatLimit || !ordinaryPageHeaders(info.HTTPHeaders, pageURL) {
		return false
	}
	formats := make([]videoFormat, 0, len(info.Formats))
	seen := make(map[string]bool, len(info.Formats))
	var preferred videoFormat
	for _, format := range info.Formats {
		if !directPageFormat(format, pageURL, pageOrigin) {
			return false
		}
		format.FormatID = fmt.Sprintf("page-%x", sha256.Sum256([]byte(format.URL+"\n"+format.Ext)))
		location, _ := url.Parse(format.URL)
		format.FormatNote = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, path.Base(location.Path))
		format.HTTPHeaders, format.Fragments = nil, nil
		if !seen[format.FormatID] {
			formats = append(formats, format)
			seen[format.FormatID] = true
		}
		if format.URL == info.URL && format.Ext == info.Ext {
			preferred = format
		}
	}
	if preferred.FormatID == "" {
		return false
	}
	info.WebpageURL = pageURL
	if origin, _ := headerOrigin(info.Thumbnail); origin != pageOrigin {
		info.Thumbnail = ""
	}
	// The allowed formats are complete media files. No subtitle streams or
	// extractor-supplied headers are needed by the direct download invocation.
	info.Subtitles, info.AutomaticCaptions, info.HTTPHeaders = nil, nil, nil
	info.Formats, info.FormatID = formats, preferred.FormatID
	return true
}

func ordinaryPageHeaders(headers map[string]string, pageURL string) bool {
	for key, value := range headers {
		if strings.ContainsAny(value, "\r\n") {
			return false
		}
		switch strings.ToLower(key) {
		case "referer":
			if validReferer(value) != pageURL {
				return false
			}
		case "accept", "accept-language", "sec-fetch-mode", "user-agent":
		default:
			return false
		}
	}
	return true
}

func directPageFormat(f videoFormat, pageURL, pageOrigin string) bool {
	if f.HasDRM || f.ManifestURL != "" || (len(f.Fragments) != 0 && string(f.Fragments) != "null") || f.VCodec == "none" || !ordinaryPageHeaders(f.HTTPHeaders, pageURL) {
		return false
	}
	u, err := url.Parse(f.URL)
	origin, _ := headerOrigin(f.URL)
	if err != nil || origin != pageOrigin || f.Protocol != u.Scheme || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(u.Path), "."))
	switch f.Ext {
	case "mp4", "m4v", "webm", "mkv", "mov":
		return ext == f.Ext
	case "ogv":
		return ext == "ogv" || ext == "ogg"
	default:
		return false
	}
}

func pageVideoInput(req *base.Request, extra *model.ReqExtra, info *videoInfo) (*base.Request, *model.ReqExtra, []byte, error) {
	if info == nil || info.ID != extra.PageCandidateID {
		return nil, nil, nil, errors.New(stalePageCandidate().Reason)
	}
	for _, format := range info.Formats {
		if format.FormatID != info.FormatID {
			continue
		}
		commandReq, commandExtra := *req, *extra
		commandReq.URL = format.URL
		commandExtra.SourcePage, commandExtra.Format = info.WebpageURL, format.FormatID
		// Only the validated direct source is passed to yt-dlp. In particular,
		// webpage_url would allow --load-info-json to re-extract the entire page.
		input, err := json.Marshal(map[string]any{
			"id": info.ID, "title": info.Title, "url": format.URL, "protocol": format.Protocol,
			"ext": format.Ext, "format_id": format.FormatID, "vcodec": format.VCodec, "acodec": format.ACodec,
		})
		return &commandReq, &commandExtra, input, err
	}
	return nil, nil, nil, errors.New(stalePageCandidate().Reason)
}
