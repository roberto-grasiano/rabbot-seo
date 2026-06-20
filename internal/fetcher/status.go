package fetcher

import "github.com/roberto-grasiano/rabbot-seo/internal/model"

// StatusTypeFor maps an HTTP status code (plus whether the request was redirected)
// to the coarse model.StatusType used by the urls table.
func StatusTypeFor(status int, redirected bool) model.StatusType {
	switch {
	case status == 0:
		return model.StatusUnreachable
	case status >= 300 && status < 400:
		return model.StatusRedirect
	case redirected && status >= 200 && status < 300:
		return model.StatusRedirect
	case status >= 200 && status < 300:
		return model.StatusPage
	case status >= 400 && status < 500:
		return model.StatusMissing
	case status >= 500:
		return model.StatusServerError
	default:
		return model.StatusUnreachable
	}
}
