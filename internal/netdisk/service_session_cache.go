package netdisk

import (
	"strings"
	"time"

	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

// rememberSessionCache merges only Quark/UC share authorization learned by a
// provider into the original in-memory session. It never persists a Capture.
// Callers with an explicit operation source should pass it (including deferred
// resolves: defer s.rememberSessionCache(ss, e.Source)). Without that argument,
// the caller must have kept the operation bound to the captured source.
func (s *Service) rememberSessionCache(ss *Session, sources ...model.Source) {
	if ss == nil || ss.ID == "" || ss.CreatedAt.IsZero() ||
		(ss.Capture.Provider != "quark" && ss.Capture.Provider != "uc") {
		return
	}
	src, ok := s.sessionCacheSource(ss)
	if !ok || src.ShareID == "" {
		return
	}
	for _, operation := range sources {
		if !sameSource(src, operation) || src.Password != operation.Password {
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.sessions[ss.ID]
	if current == nil || !current.CreatedAt.Equal(ss.CreatedAt) || time.Since(current.CreatedAt) >= 8*time.Hour ||
		current.Capture.Provider != ss.Capture.Provider || current.Capture.AccountID != ss.Capture.AccountID ||
		current.Capture.SourcePage != ss.Capture.SourcePage || current.Capture.InstanceID != ss.Capture.InstanceID ||
		current.Capture.ContextID != ss.Capture.ContextID {
		return
	}
	bound, ok := s.sessionCacheSource(current)
	if !ok || bound != src {
		return
	}
	// FID tokens belong to the share-token generation that produced them. Never
	// combine them with a newer capture or another operation's different stoken.
	stoken := sessionValue(ss, "stoken")
	if stoken == "" {
		return
	}
	if existing := sessionValue(current, "stoken"); existing != "" && existing != stoken {
		return
	}
	for key, value := range ss.Capture.Share {
		fid := strings.TrimPrefix(key, "share_fid_token.")
		isFID := fid != key && fid != ""
		if value == "" || (key != "stoken" && !isFID) {
			continue
		}
		// Existing captured values (including browser file metadata and aliases)
		// take precedence over late results from a cloned operation.
		if sessionValue(current, key) != "" {
			continue
		}
		if key != "stoken" && shareFID(current, fid) != "" {
			continue
		}
		remember(current, key, value)
	}
}

func (s *Service) sessionCacheSource(ss *Session) (model.Source, bool) {
	src, err := s.Detect(ss.Capture.SourcePage)
	if err != nil || src.Provider != ss.Capture.Provider {
		return model.Source{}, false
	}
	if share := ss.Capture.Share["shareId"]; share != "" {
		src.ShareID = share
	}
	if password := ss.Capture.Share["password"]; password != "" {
		src.Password = password
	}
	// Parent folders vary during a walk, but all tokens remain share scoped.
	src.ParentID = ""
	return src, true
}
