package netdisk

import (
	"net/http"
	"testing"
)

func TestRangePolicyDefersRedirectValidation(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		req, _ := http.NewRequest("GET", "https://d.pcs.baidu.com/fixture", nil)
		req.Header.Set("Range", "bytes=0-2")
		res := &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://cdn.baidupcs.com/fixture"}}}
		if err := rangeResponse(3)(req, res); err != nil {
			t.Errorf("redirect %d rejected before final range response: %v", status, err)
		}
	}
}
