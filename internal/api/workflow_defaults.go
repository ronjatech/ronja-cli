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
	// Language echoes the language asked for. An instance that predates
	// JavaScript workflows ignores ?language and omits this, which is how a js
	// request learns the instance cannot create one.
	Language          string `json:"language,omitempty"`
	RuntimeVersion    int    `json:"runtimeVersion"`
	Entrypoint        string `json:"entrypoint,omitempty"`
	EntrypointStarter string `json:"entrypointStarter"`
	// PackageJSON is the starter package.json of a js workflow; absent for python.
	PackageJSON string `json:"packageJSON,omitempty"`
}

// GetWorkflowDefaults reads the instance's defaults for a new workflow in
// language on runtime, or on the instance's default runtime when runtime is 0.
// An empty language is left out, so a python request is the one every instance
// has always answered.
//
// Zero leaves the parameter OUT: the server reads an absent one as "the
// default" and refuses `runtime=0`. A runtime a workflow cannot be created on is
// a 400 whose text names the ones it can, and it comes back as it is, like every
// other failure — the 404 of an instance that predates the route included,
// which the caller words.
func (c *Client) GetWorkflowDefaults(ctx context.Context, runtime int, language string) (*WorkflowDefaults, error) {
	path := "workflow/defaults"
	query := url.Values{}
	if language != "" {
		query.Set("language", language)
	}
	if runtime != 0 {
		query.Set("runtime", strconv.Itoa(runtime))
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var out WorkflowDefaults
	if err := c.Do(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
