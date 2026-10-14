package media

// Cookie matches the browser cookies API fields used by the browser bridge.
type Cookie struct {
	Name           string  `json:"name"`
	Value          string  `json:"value"`
	Domain         string  `json:"domain"`
	Path           string  `json:"path"`
	Secure         bool    `json:"secure"`
	HTTPOnly       bool    `json:"httpOnly"`
	ExpirationDate float64 `json:"expirationDate"`
	HostOnly       bool    `json:"hostOnly"`
}

type HeaderScope struct {
	Origin     string            `json:"origin,omitempty"`
	URL        string            `json:"url,omitempty"`
	PathPrefix string            `json:"pathPrefix,omitempty"`
	Headers    map[string]string `json:"headers"`
}

type Subtitle struct {
	Language  string   `json:"language"`
	Name      string   `json:"name"`
	Automatic bool     `json:"automatic"`
	Formats   []string `json:"formats,omitempty"`
}

type ReqExtra struct {
	CollectContext          bool              `json:"collectContext"`
	SubtitleLanguages       []string          `json:"subtitleLanguages"`
	WriteAutomaticSubtitles bool              `json:"writeAutomaticSubtitles"`
	EmbedSubtitles          bool              `json:"embedSubtitles"`
	ScopedHeaders           []HeaderScope     `json:"scopedHeaders"`
	Header                  map[string]string `json:"header"`
	Cookies                 []Cookie          `json:"cookies"`
	Format                  string            `json:"format"`
	Title                   string            `json:"title"`
	SourcePage              string            `json:"sourcePage"`
	Container               string            `json:"container"`
	ContextID               string            `json:"contextId,omitempty"`
	SourceType              string            `json:"sourceType,omitempty"`
	ExpectedMediaID         string            `json:"expectedMediaId,omitempty"`
	// Empty preserves the behavior of tasks created before explicit track intent.
	MediaKind string `json:"mediaKind,omitempty"`
}

type Config struct {
	YtDlpPath     string `json:"ytDlpPath"`
	FFmpegPath    string `json:"ffmpegPath"`
	JsRuntimePath string `json:"jsRuntimePath,omitempty"`
	Connections   int    `json:"connections"`
}

type Format struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	Ext         string  `json:"ext"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	VCodec      string  `json:"vcodec"`
	ACodec      string  `json:"acodec"`
	FileSize    int64   `json:"filesize"`
	Container   string  `json:"container"`
	Protocol    string  `json:"protocol,omitempty"`
	Language    string  `json:"language,omitempty"`
	FPS         float64 `json:"fps,omitempty"`
	TBR         float64 `json:"tbr,omitempty"`
	ManifestURL string  `json:"manifestUrl,omitempty"`
	SourceURL   string  `json:"sourceUrl,omitempty"`
}

type Inspection struct {
	ContextURLs []string   `json:"contextUrls"`
	Thumbnail   string     `json:"thumbnail"`
	Subtitles   []Subtitle `json:"subtitles"`
	ID          string     `json:"id,omitempty"`
	MediaID     string     `json:"mediaId,omitempty"`
	SourceURL   string     `json:"sourceUrl,omitempty"`
	WebpageURL  string     `json:"webpageUrl,omitempty"`
	ManifestURL string     `json:"manifestUrl,omitempty"`
	Protocol    string     `json:"protocol,omitempty"`
	Title       string     `json:"title"`
	Duration    float64    `json:"duration"`
	Formats     []Format   `json:"formats"`
	Supported   bool       `json:"supported"`
	Reason      string     `json:"reason,omitempty"`
	IsLive      bool       `json:"isLive"`
}

type Stats struct {
	Phase      string `json:"phase"`
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	Format     string `json:"format"`
}
