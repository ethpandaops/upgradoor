package registry

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// addImage stores a configuration with labels and an image manifest that
// names it, serves the configuration as a blob and returns the manifest's
// digest.
func (f *fakeRegistry) addImage(labels string) string {
	config := []byte(`{"config":{"Labels":` + labels + `}}`)
	digest := f.add(config)

	f.handle("/blobs/"+digest, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(config)
	})

	return f.add([]byte(`{"config":{"digest":"` + digest + `"}}`))
}

func TestLabelsOfAnImage(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	image := f.addImage(`{"commit":"abc1234","ethpandaops.io.repo":"org/src"}`)

	labels, err := r.Labels(t.Context(), f.ref(tagSingle), image)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"commit": "abc1234", "ethpandaops.io.repo": "org/src"}, labels)
}

func TestLabelsOfAnIndexAreThoseOfLinuxAmd64(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	arm := f.addImage(`{"commit":"arm"}`)
	amd := f.addImage(`{"commit":"amd"}`)
	index := f.add([]byte(`{"manifests":[` +
		`{"digest":"sha256:` + "00" + `","platform":{"os":"unknown","architecture":"unknown"}},` +
		`{"digest":"` + arm + `","platform":{"os":"linux","architecture":"arm64"}},` +
		`{"digest":"` + amd + `","platform":{"os":"linux","architecture":"amd64"}}]}`))

	labels, err := r.Labels(t.Context(), f.ref(tagMulti), index)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"commit": "amd"}, labels)

	// Without an amd64 image the first image counts, never an attestation.
	index = f.add([]byte(`{"manifests":[` +
		`{"digest":"` + amd + `","platform":{"os":"unknown","architecture":"unknown"}},` +
		`{"digest":"` + arm + `","platform":{"os":"linux","architecture":"arm64"}}]}`))

	labels, err = r.Labels(t.Context(), f.ref(tagMulti), index)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"commit": "arm"}, labels)
}

func TestLabelsErrors(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	_, err := r.Labels(t.Context(), f.ref(tagSingle), "not-a-digest")
	require.ErrorContains(t, err, "is not a digest")

	// The fixture's manifest names a configuration that is no digest.
	_, err = r.Labels(t.Context(), f.ref(tagSingle), f.amd64Digest)
	require.ErrorContains(t, err, "names no configuration")

	_, err = r.Labels(t.Context(), f.ref(tagSingle), sha256Digest([]byte("absent")))
	require.True(t, IsNotFound(err))

	_, err = r.Resolve(t.Context(), f.ref("absent"))
	require.True(t, IsNotFound(err))
	require.False(t, IsNotFound(nil))
}
