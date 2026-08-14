// SPDX-License-Identifier: MIT

package oci

import (
	"context"
	"testing"

	"github.com/kumabox/kumabox/internal/image"
)

type fakeContentStore struct {
	result *PullResult
	req    PullRequest
}

func (f *fakeContentStore) Pull(_ context.Context, req PullRequest) (*PullResult, error) {
	f.req = req
	return f.result, nil
}

type fakeImageBuilder struct {
	result *image.ImageRecord
	req    BuildRequest
}

func (f *fakeImageBuilder) Build(_ context.Context, req BuildRequest) (*image.ImageRecord, error) {
	f.req = req
	return f.result, nil
}

func TestImagePipelineDelegatesWorkflowSteps(t *testing.T) {
	ref := "registry.example/test:latest"
	pullResult := &PullResult{Ref: ref}
	imageResult := &image.ImageRecord{Name: "test"}
	content := &fakeContentStore{result: pullResult}
	builder := &fakeImageBuilder{result: imageResult}
	pipeline := &ImagePipeline{content: content, builder: builder}

	pulled, err := pipeline.Pull(t.Context(), PullRequest{Ref: ref})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if pulled != pullResult || content.req.Ref != ref {
		t.Fatalf("pull was not delegated: result=%#v request=%#v", pulled, content.req)
	}

	built, err := pipeline.Build(t.Context(), BuildRequest{Name: imageResult.Name, Ref: ref})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built != imageResult || builder.req.Name != imageResult.Name {
		t.Fatalf("build was not delegated: result=%#v request=%#v", built, builder.req)
	}
}
