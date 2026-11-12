package http

// PathFreezeMIME returns only the actual resolved response's Content-Type.
// It performs no sniffing, extension inference, network or persistence.
func (f *Fetcher) PathFreezeMIME() string {
	f.resolveRespLock.Lock()
	defer f.resolveRespLock.Unlock()
	if f.resolveResp == nil {
		return ""
	}
	return f.resolveResp.Header.Get("Content-Type")
}
