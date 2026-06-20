package control

import "context"

// AddSite registers a new monitored site (POST /v1/sites) and returns the new id.
//
// NOTE (M1 plan gap): the M1 plan's CLI tasks (Task 20 runSitesAdd, Task 21
// runCrawl) call (*Client).AddSite / (*Client).Crawl, but Task 19 only added
// Pause/Resume/RemoveSite/SetConfig/Status. The control server already exposes
// POST /v1/sites and POST /v1/crawl (handleAddSite/handleCrawl); these two
// methods mirror the plan's own do/post round-trip idiom so the CLI compiles.
func (c *Client) AddSite(ctx context.Context, req AddSiteRequest) (AddSiteResponse, error) {
	var resp AddSiteResponse
	if err := c.post(ctx, "/v1/sites", req, &resp); err != nil {
		return AddSiteResponse{}, err
	}
	return resp, nil
}

// Crawl forces an immediate recheck of a URL or site (POST /v1/crawl).
func (c *Client) Crawl(ctx context.Context, req CrawlRequest) (CrawlResponse, error) {
	var resp CrawlResponse
	if err := c.post(ctx, "/v1/crawl", req, &resp); err != nil {
		return CrawlResponse{}, err
	}
	return resp, nil
}
