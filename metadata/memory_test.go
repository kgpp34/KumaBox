package metadata_test

import (
	"testing"

	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/metadata/metadatatest"
)

func TestMemoryContract(t *testing.T) {
	metadatatest.Run(t, func(t *testing.T, collections []metadata.Collection) metadata.Store {
		store, err := metadata.NewMemory(collections)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
		return store
	})
}
