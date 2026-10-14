package media

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

//go:embed context.py
var contextWrapper string

func cloneRequest(req *base.Request) (*base.Request, error) {
	if req == nil {
		return nil, errors.New("missing media request")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, errors.New("invalid media request")
	}
	var out base.Request
	if json.Unmarshal(data, &out) != nil {
		return nil, errors.New("invalid media request")
	}
	if _, err := extraFor(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func headerOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("invalid media header origin")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" && !(u.Scheme == "http" && port == "80") && !(u.Scheme == "https" && port == "443") {
		host += ":" + port
	}
	return u.Scheme + "://" + host, nil
}

func contextScopes(req *base.Request, extra *model.ReqExtra) ([]model.HeaderScope, error) {
	origin, err := headerOrigin(req.URL)
	if err != nil {
		return nil, err
	}
	scopes := make([]model.HeaderScope, 0, len(extra.ScopedHeaders)+2)
	headers := map[string]string{}
	browserHeaders := map[string]string{}
	for k, v := range extra.Header {
		switch {
		case strings.EqualFold(k, "Referer"):
			if referer := validReferer(v); referer != "" {
				browserHeaders["Referer"] = referer
			}
		case strings.EqualFold(k, "User-Agent"):
			browserHeaders["User-Agent"] = v
		default:
			headers[k] = v
		}
	}
	if referer := validReferer(extra.SourcePage); referer != "" {
		browserHeaders["Referer"] = referer
	}
	// Browser navigation context applies to dependent requests on the media origin.
	// Tokens and arbitrary headers retain exact-URL scope; foreign origins require
	// explicit scopes and receive neither inherited Referer nor User-Agent.
	scopes = append(scopes, model.HeaderScope{URL: req.URL, Headers: headers})
	scopes = append(scopes, model.HeaderScope{Origin: origin, Headers: browserHeaders})
	scopes = append(scopes, extra.ScopedHeaders...)
	for i, scope := range scopes {
		if scope.URL != "" {
			if _, err := headerOrigin(scope.URL); err != nil {
				return nil, err
			}
			scope.Origin = ""
			scope.PathPrefix = ""
		} else {
			normalized, err := headerOrigin(scope.Origin)
			if err != nil {
				return nil, err
			}
			u, _ := url.Parse(scope.Origin)
			if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
				return nil, errors.New("header scope must be an origin")
			}
			scope.Origin = normalized
			if scope.PathPrefix != "" && (!strings.HasPrefix(scope.PathPrefix, "/") || strings.ContainsAny(scope.PathPrefix, "?#\r\n")) {
				return nil, errors.New("invalid header path prefix")
			}
		}
		clean := map[string]string{}
		for k, v := range scope.Headers {
			lower := strings.ToLower(k)
			if k == "" || strings.ContainsAny(k, " \t\r\n:") || strings.ContainsAny(v, "\r\n") {
				return nil, errors.New("invalid scoped media header")
			}
			switch lower {
			case "cookie", "host", "proxy-authorization", "proxyauthorization", "connection", "content-length":
				continue
			}
			// Legacy auth was never supported. Require explicit origin authority.
			if i == 0 && lower == "authorization" {
				continue
			}
			clean[k] = v
		}
		scope.Headers = clean
		scopes[i] = scope
	}
	return scopes, nil
}

func validReferer(raw string) string {
	if strings.ContainsAny(raw, "\r\n") {
		return ""
	}
	if _, err := headerOrigin(raw); err != nil {
		return ""
	}
	parsed, _ := url.Parse(raw)
	parsed.Fragment = ""
	return parsed.String()
}

// Use the configured entrypoint's interpreter, never a different yt-dlp installation.
func mediaCommand(ctx context.Context, cfg model.Config, req *base.Request, extra *model.ReqExtra, args []string) (*exec.Cmd, func(), error) {
	scopes, err := contextScopes(req, extra)
	if err != nil {
		return nil, nil, err
	}
	tool, err := exec.LookPath(cfg.YtDlpPath)
	var interpreter string
	if err == nil {
		if f, openErr := os.Open(tool); openErr == nil {
			b := make([]byte, 1024)
			n, _ := f.Read(b)
			_ = f.Close()
			line, _, _ := strings.Cut(string(b[:n]), "\n")
			candidate := strings.TrimSpace(strings.TrimPrefix(line, "#!"))
			if strings.HasPrefix(line, "#!") && filepath.IsAbs(candidate) && strings.HasPrefix(filepath.Base(candidate), "python") && executablePath(candidate) {
				interpreter = candidate
			}
		}
	}
	if interpreter == "" {
		return standaloneCommand(ctx, cfg, extra, scopes, args)
	}
	payload, err := json.Marshal(struct {
		Args   []string            `json:"args"`
		Scopes []model.HeaderScope `json:"scopes"`
		Probe  string              `json:"probe"`
	}{args, scopes, probePath(cfg)})
	if err != nil {
		return nil, nil, errors.New("invalid media context")
	}
	file, err := os.CreateTemp("", "gopeed-media-context-*")
	if err != nil {
		return nil, nil, errors.New("cannot create media context")
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(payload)
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		cleanup()
		return nil, nil, errors.New("cannot secure media context")
	}
	cmd := exec.CommandContext(ctx, interpreter, "-c", contextWrapper, file.Name())
	cmd.Env = cleanEnvironment()
	return cmd, cleanup, nil
}

// Standalone yt-dlp can carry ordinary browser context and domain cookies, but
// cannot enforce exact URL/origin header scopes. Never broadcast scoped tokens.
func standaloneCommand(ctx context.Context, cfg model.Config, extra *model.ReqExtra, scopes []model.HeaderScope, args []string) (*exec.Cmd, func(), error) {
	unsupported := errors.New("standalone yt-dlp does not support strict scoped authentication headers; configure a Python yt-dlp entrypoint")
	for key := range extra.Header {
		if !strings.EqualFold(key, "Referer") && !strings.EqualFold(key, "User-Agent") {
			return nil, nil, unsupported
		}
	}
	for _, scope := range extra.ScopedHeaders {
		if len(scope.Headers) != 0 {
			return nil, nil, unsupported
		}
	}
	var private, public []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--proxy" && i+1 < len(args) {
			private = append(private, args[i], args[i+1])
			i++
		} else {
			public = append(public, args[i])
		}
	}
	for key, value := range scopes[1].Headers {
		private = append(private, "--add-headers", key+":"+value)
	}
	file, err := os.CreateTemp("", "gopeed-media-cli-*")
	if err != nil {
		return nil, nil, errors.New("cannot create media config")
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	// yt-dlp config files use POSIX shlex quoting, not shell evaluation.
	var content strings.Builder
	for _, arg := range private {
		content.WriteString("'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'\n")
	}
	if err = file.Chmod(0600); err == nil {
		_, err = file.WriteString(content.String())
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		cleanup()
		return nil, nil, errors.New("cannot secure media config")
	}
	// --ignore-config suppresses ambient configs, but permits explicit locations.
	public = append([]string{"--ignore-config", "--config-locations", file.Name()}, public...)
	cmd := exec.CommandContext(ctx, cfg.YtDlpPath, public...)
	cmd.Env = cleanEnvironment()
	return cmd, cleanup, nil
}
