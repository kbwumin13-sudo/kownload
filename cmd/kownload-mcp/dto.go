package main

// No URL, name, path, request headers, arbitrary error message or extra map is
// part of this contract. Additional fields require a versioned contract review.
type Progress struct {
	DownloadedBytes           int64  `json:"downloadedBytes"`
	SpeedBytesPerSecond       int64  `json:"speedBytesPerSecond"`
	UploadedBytes             int64  `json:"uploadedBytes"`
	UploadSpeedBytesPerSecond int64  `json:"uploadSpeedBytesPerSecond"`
	TotalBytes                *int64 `json:"totalBytes"`
	Phase                     string `json:"phase"`
}
type Task struct {
	ID        string   `json:"id"`
	Protocol  string   `json:"protocol"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"createdAt"`
	UpdatedAt string   `json:"updatedAt"`
	Progress  Progress `json:"progress"`
}
type TaskList struct {
	Tasks      []Task `json:"tasks"`
	NextCursor string `json:"nextCursor"`
}
type Stats struct {
	Available     bool   `json:"available"`
	ActiveStreams *int64 `json:"activeStreams,omitempty"`
	TargetStreams *int64 `json:"targetStreams,omitempty"`
	Peers         *int64 `json:"peers,omitempty"`
}
type LastError struct {
	Available  bool   `json:"available"`
	Category   string `json:"category,omitempty"`
	Retryable  *bool  `json:"retryable,omitempty"`
	OccurredAt string `json:"occurredAt,omitempty"`
}
