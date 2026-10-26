package netdisk

import "time"

// Source identifies a cloud drive page without a resolved, expiring download URL.
type Source struct {
	Provider string `json:"provider"`
	URL      string `json:"url"`
	ShareID  string `json:"shareId,omitempty"`
	Password string `json:"password,omitempty"`
	ParentID string `json:"parentId,omitempty"`
}

type File struct {
	ID    string            `json:"id"`
	Name  string            `json:"name"`
	Path  string            `json:"path,omitempty"`
	Size  int64             `json:"size"`
	Hash  string            `json:"hash,omitempty"`
	IsDir bool              `json:"isDir"`
	Meta  map[string]string `json:"meta,omitempty"`
}

type Cookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Secure  bool    `json:"secure"`
	Expires float64 `json:"expires,omitempty"`
}

// Capture is accepted only through the authenticated local browser bridge.
// It is kept in memory, never in task snapshots or protocol state.
type Capture struct {
	Provider   string            `json:"provider"`
	SourcePage string            `json:"sourcePage"`
	Browser    string            `json:"browser"`
	InstanceID string            `json:"instanceId"`
	ContextID  string            `json:"contextId"`
	AccountID  string            `json:"accountId,omitempty"`
	Cookies    []Cookie          `json:"cookies,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Tokens     map[string]string `json:"tokens,omitempty"`
	Share      map[string]string `json:"share,omitempty"`
	Files      []File            `json:"files,omitempty"`
}

type ReqExtra struct {
	Provider  string `json:"provider"`
	Source    Source `json:"source"`
	File      File   `json:"file"`
	SessionID string `json:"sessionId,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	BatchID   string `json:"batchId,omitempty"`
	// Deferred binds a shared-directory task to its durable batch manifest.
	Deferred bool `json:"deferred,omitempty"`
}

type Listing struct {
	Source    Source `json:"source"`
	SessionID string `json:"sessionId,omitempty"`
	Files     []File `json:"files"`
	NextPage  string `json:"nextPage,omitempty"`
	Title     string `json:"title,omitempty"`
}

type ProviderInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Hosts    []string `json:"hosts"`
	APIHosts []string `json:"apiHosts"`
}

type BrowserRequest struct {
	ID         string    `json:"id"`
	Source     Source    `json:"source"`
	InstanceID string    `json:"instanceId,omitempty"`
	ExpiresAt  time.Time `json:"expiresAt"`
	State      string    `json:"state"`
	SessionID  string    `json:"sessionId,omitempty"`
	Message    string    `json:"message,omitempty"`
}

type TransferReceipt struct {
	BatchID    string   `json:"batchId"`
	Provider   string   `json:"provider"`
	AccountID  string   `json:"accountId"`
	FolderID   string   `json:"folderId,omitempty"`
	CreatedIDs []string `json:"createdIds"`
	Files      []File   `json:"files"`
	Source     Source   `json:"source"`
	Remaining  []string `json:"remaining"`
	State      string   `json:"state"`
	Message    string   `json:"message,omitempty"`
}
