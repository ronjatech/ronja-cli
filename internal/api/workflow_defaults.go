package api

import (
	"context"
	"net/url"
	"strconv"
)

// WorkflowDefaults is what GET /workflow/defaults answers: the runtime an
// instance creates a new workflow on, and the entrypoint text it starts one
// from. `wf init` pins both into a new folder, and the first push of a folder
// that names no runtime pins the runtime, so every create names the runtime its
// code is written for.
type WorkflowDefaults struct {
	RuntimeVersion    int    `json:"runtimeVersion"`
	EntrypointStarter string `json:"entrypointStarter"`
}

// GetWorkflowDefaults reads the instance's defaults for a new workflow on
// runtime, or on the instance's default runtime when runtime is 0.
//
// Zero leaves the parameter OUT: the server reads an absent one as "the
// default" and refuses `runtime=0`. A runtime a workflow cannot be created on is
// a 400 whose text names the ones it can, and it comes back as it is, like every
// other failure — the 404 of an instance that predates the route included,
// which the caller words.
func (c *Client) GetWorkflowDefaults(ctx context.Context, runtime int) (*WorkflowDefaults, error) {
	path := "workflow/defaults"
	if runtime != 0 {
		path += "?" + url.Values{"runtime": {strconv.Itoa(runtime)}}.Encode()
	}
	var out WorkflowDefaults
	if err := c.Do(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
