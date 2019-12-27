//nolint (dupl)
package v1_0_0

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/anuvu/zot/pkg/api"
	"github.com/anuvu/zot/pkg/compliance"
	godigest "github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/smartystreets/goconvey/convey/reporting"
	"gopkg.in/resty.v1"
)

func Location(baseURL string, resp *resty.Response, config *compliance.Config) string {
	// For some API responses, the Location header is set and is supposed to
	// indicate an opaque value. However, it is not clear if this value is an
	// absolute URL (https://server:port/v2/...) or just a path (/v2/...)
	// zot implements the latter as per the spec, but some registries appear to
	// return the former - this needs to be clarified
	loc := resp.Header().Get("Location")
	if config.Compliance {
		return loc
	}

	return baseURL + loc
}

func CheckWorkflows(t *testing.T, config *compliance.Config) {
	if config == nil || config.Address == "" || config.Port == "" {
		panic("insufficient config")
	}

	if config.OutputJSON {
		outputJSONEnter()
		defer outputJSONExit()
	}

	baseURL := fmt.Sprintf("http://%s:%s", config.Address, config.Port)

	fmt.Println("------------------------------")
	fmt.Println("Checking for v1.0.0 compliance")
	fmt.Println("------------------------------")

	Convey("Make API calls to the controller", t, func(c C) {
		Convey("Check version", func() {
			Print("\nCheck version")
			resp, err := resty.R().Get(baseURL + "/v2/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
		})

		Convey("Get repository catalog", func() {
			Print("\nGet repository catalog")
			resp, err := resty.R().Get(baseURL + "/v2/_catalog")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
			So(resp.String(), ShouldNotBeEmpty)
			So(resp.Header().Get("Content-Type"), ShouldEqual, api.DefaultMediaType)
			var repoList api.RepositoryList
			err = json.Unmarshal(resp.Body(), &repoList)
			So(err, ShouldBeNil)
			So(len(repoList.Repositories), ShouldEqual, 0)

			// after newly created upload should succeed
			resp, err = resty.R().Post(baseURL + "/v2/z/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)

			// after newly created upload should succeed
			resp, err = resty.R().Post(baseURL + "/v2/a/b/c/d/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)

			resp, err = resty.R().SetResult(&api.RepositoryList{}).Get(baseURL + "/v2/_catalog")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
			So(resp.String(), ShouldNotBeEmpty)
			r := resp.Result().(*api.RepositoryList)
			if !config.Compliance {
				// stricter check for zot ci/cd
				So(len(r.Repositories), ShouldBeGreaterThan, 0)
				So(r.Repositories[0], ShouldEqual, "a/b/c/d")
				So(r.Repositories[1], ShouldEqual, "z")
			}
		})

		Convey("Get images in a repository", func() {
			Print("\nGet images in a repository")
			// non-existent repository should fail
			resp, err := resty.R().Get(baseURL + "/v2/repo1/tags/list")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 404)
			So(resp.String(), ShouldNotBeEmpty)

			// after newly created upload should succeed
			resp, err = resty.R().Post(baseURL + "/v2/repo1/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)

			resp, err = resty.R().Get(baseURL + "/v2/repo1/tags/list")
			So(err, ShouldBeNil)
			if !config.Compliance {
				// stricter check for zot ci/cd
				So(resp.StatusCode(), ShouldEqual, 200)
				So(resp.String(), ShouldNotBeEmpty)
			}
		})

		Convey("Monolithic blob upload", func() {
			Print("\nMonolithic blob upload")
			resp, err := resty.R().Post(baseURL + "/v2/repo2/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
			loc := Location(baseURL, resp, config)
			So(loc, ShouldNotBeEmpty)

			resp, err = resty.R().Get(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 204)

			resp, err = resty.R().Get(baseURL + "/v2/repo2/tags/list")
			So(err, ShouldBeNil)
			if !config.Compliance {
				// stricter check for zot ci/cd
				So(resp.StatusCode(), ShouldEqual, 200)
				So(resp.String(), ShouldNotBeEmpty)
			}

			// without a "?digest=<>" should fail
			content := []byte("this is a blob")
			digest := godigest.FromBytes(content)
			So(digest, ShouldNotBeNil)
			resp, err = resty.R().Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 400)
			// without the Content-Length should fail
			resp, err = resty.R().SetQueryParam("digest", digest.String()).Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 400)
			// without any data to send, should fail
			resp, err = resty.R().SetQueryParam("digest", digest.String()).
				SetHeader("Content-Type", "application/octet-stream").Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 400)
			// monolithic blob upload: success
			resp, err = resty.R().SetQueryParam("digest", digest.String()).
				SetHeader("Content-Type", "application/octet-stream").SetBody(content).Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 201)
			blobLoc := Location(baseURL, resp, config)
			So(blobLoc, ShouldNotBeEmpty)
			So(resp.Header().Get("Content-Length"), ShouldEqual, "0")
			So(resp.Header().Get(api.DistContentDigestKey), ShouldNotBeEmpty)
			// upload reference should now be removed
			resp, err = resty.R().Get(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 404)
			// blob reference should be accessible
			resp, err = resty.R().Get(blobLoc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
		})

		Convey("Monolithic blob upload with multiple name components", func() {
			Print("\nMonolithic blob upload with multiple name components")
			resp, err := resty.R().Post(baseURL + "/v2/repo10/repo20/repo30/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
			loc := Location(baseURL, resp, config)
			So(loc, ShouldNotBeEmpty)

			resp, err = resty.R().Get(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 204)

			resp, err = resty.R().Get(baseURL + "/v2/repo10/repo20/repo30/tags/list")
			So(err, ShouldBeNil)
			if !config.Compliance {
				// stricter check for zot ci/cd
				So(resp.StatusCode(), ShouldEqual, 200)
				So(resp.String(), ShouldNotBeEmpty)
			}

			// without a "?digest=<>" should fail
			content := []byte("this is a blob")
			digest := godigest.FromBytes(content)
			So(digest, ShouldNotBeNil)
			resp, err = resty.R().Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 400)
			// without the Content-Length should fail
			resp, err = resty.R().SetQueryParam("digest", digest.String()).Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 400)
			// without any data to send, should fail
			resp, err = resty.R().SetQueryParam("digest", digest.String()).
				SetHeader("Content-Type", "application/octet-stream").Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 400)
			// monolithic blob upload: success
			resp, err = resty.R().SetQueryParam("digest", digest.String()).
				SetHeader("Content-Type", "application/octet-stream").SetBody(content).Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 201)
			blobLoc := Location(baseURL, resp, config)
			So(blobLoc, ShouldNotBeEmpty)
			So(resp.Header().Get("Content-Length"), ShouldEqual, "0")
			So(resp.Header().Get(api.DistContentDigestKey), ShouldNotBeEmpty)
			// upload reference should now be removed
			resp, err = resty.R().Get(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 404)
			// blob reference should be accessible
			resp, err = resty.R().Get(blobLoc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
		})

		Convey("Chunked blob upload", func() {
			Print("\nChunked blob upload")
			resp, err := resty.R().Post(baseURL + "/v2/repo3/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
			loc := Location(baseURL, resp, config)
			So(loc, ShouldNotBeEmpty)

			var buf bytes.Buffer
			chunk1 := []byte("this is the first chunk")
			n, err := buf.Write(chunk1)
			So(n, ShouldEqual, len(chunk1))
			So(err, ShouldBeNil)

			// write first chunk
			contentRange := fmt.Sprintf("%d-%d", 0, len(chunk1))
			resp, err = resty.R().SetHeader("Content-Type", "application/octet-stream").
				SetHeader("Content-Range", contentRange).SetBody(chunk1).Patch(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)

			// check progress
			resp, err = resty.R().Get(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 204)
			r := resp.Header().Get("Range")
			So(r, ShouldNotBeEmpty)
			So(r, ShouldEqual, "bytes="+contentRange)

			// write same chunk should fail
			contentRange = fmt.Sprintf("%d-%d", 0, len(chunk1))
			resp, err = resty.R().SetHeader("Content-Type", "application/octet-stream").
				SetHeader("Content-Range", contentRange).SetBody(chunk1).Patch(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 400)
			So(resp.String(), ShouldNotBeEmpty)

			chunk2 := []byte("this is the second chunk")
			n, err = buf.Write(chunk2)
			So(n, ShouldEqual, len(chunk2))
			So(err, ShouldBeNil)

			digest := godigest.FromBytes(buf.Bytes())
			So(digest, ShouldNotBeNil)

			// write final chunk
			contentRange = fmt.Sprintf("%d-%d", len(chunk1), len(buf.Bytes()))
			resp, err = resty.R().SetQueryParam("digest", digest.String()).
				SetHeader("Content-Range", contentRange).
				SetHeader("Content-Type", "application/octet-stream").SetBody(chunk2).Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 201)
			blobLoc := Location(baseURL, resp, config)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 201)
			So(blobLoc, ShouldNotBeEmpty)
			So(resp.Header().Get("Content-Length"), ShouldEqual, "0")
			So(resp.Header().Get(api.DistContentDigestKey), ShouldNotBeEmpty)
			// upload reference should now be removed
			resp, err = resty.R().Get(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 404)
			// blob reference should be accessible
			resp, err = resty.R().Get(blobLoc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
		})

		Convey("Chunked blob upload with multiple name components", func() {
			Print("\nChunked blob upload with multiple name components")
			resp, err := resty.R().Post(baseURL + "/v2/repo40/repo50/repo60/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
			loc := Location(baseURL, resp, config)
			So(loc, ShouldNotBeEmpty)

			var buf bytes.Buffer
			chunk1 := []byte("this is the first chunk")
			n, err := buf.Write(chunk1)
			So(n, ShouldEqual, len(chunk1))
			So(err, ShouldBeNil)

			// write first chunk
			contentRange := fmt.Sprintf("%d-%d", 0, len(chunk1))
			resp, err = resty.R().SetHeader("Content-Type", "application/octet-stream").
				SetHeader("Content-Range", contentRange).SetBody(chunk1).Patch(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)

			// check progress
			resp, err = resty.R().Get(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 204)
			r := resp.Header().Get("Range")
			So(r, ShouldNotBeEmpty)
			So(r, ShouldEqual, "bytes="+contentRange)

			// write same chunk should fail
			contentRange = fmt.Sprintf("%d-%d", 0, len(chunk1))
			resp, err = resty.R().SetHeader("Content-Type", "application/octet-stream").
				SetHeader("Content-Range", contentRange).SetBody(chunk1).Patch(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 400)
			So(resp.String(), ShouldNotBeEmpty)

			chunk2 := []byte("this is the second chunk")
			n, err = buf.Write(chunk2)
			So(n, ShouldEqual, len(chunk2))
			So(err, ShouldBeNil)

			digest := godigest.FromBytes(buf.Bytes())
			So(digest, ShouldNotBeNil)

			// write final chunk
			contentRange = fmt.Sprintf("%d-%d", len(chunk1), len(buf.Bytes()))
			resp, err = resty.R().SetQueryParam("digest", digest.String()).
				SetHeader("Content-Range", contentRange).
				SetHeader("Content-Type", "application/octet-stream").SetBody(chunk2).Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 201)
			blobLoc := Location(baseURL, resp, config)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 201)
			So(blobLoc, ShouldNotBeEmpty)
			So(resp.Header().Get("Content-Length"), ShouldEqual, "0")
			So(resp.Header().Get(api.DistContentDigestKey), ShouldNotBeEmpty)
			// upload reference should now be removed
			resp, err = resty.R().Get(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 404)
			// blob reference should be accessible
			resp, err = resty.R().Get(blobLoc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
		})

		Convey("Create and delete uploads", func() {
			Print("\nCreate and delete uploads")
			// create a upload
			resp, err := resty.R().Post(baseURL + "/v2/repo4/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
			loc := Location(baseURL, resp, config)
			So(loc, ShouldNotBeEmpty)

			// delete this upload
			resp, err = resty.R().Delete(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 204)
		})

		Convey("Create and delete blobs", func() {
			Print("\nCreate and delete blobs")
			// create a upload
			resp, err := resty.R().Post(baseURL + "/v2/repo5/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
			loc := Location(baseURL, resp, config)
			So(loc, ShouldNotBeEmpty)

			content := []byte("this is a blob")
			digest := godigest.FromBytes(content)
			So(digest, ShouldNotBeNil)
			// monolithic blob upload
			resp, err = resty.R().SetQueryParam("digest", digest.String()).
				SetHeader("Content-Type", "application/octet-stream").SetBody(content).Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 201)
			blobLoc := Location(baseURL, resp, config)
			So(blobLoc, ShouldNotBeEmpty)
			So(resp.Header().Get(api.DistContentDigestKey), ShouldNotBeEmpty)

			// delete this blob
			resp, err = resty.R().Delete(blobLoc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
			So(resp.Header().Get("Content-Length"), ShouldEqual, "0")
		})

		Convey("Mount blobs", func() {
			Print("\nMount blobs from another repository")
			// create a upload
			resp, err := resty.R().Post(baseURL + "/v2/repo6/blobs/uploads/?digest=\"abc\"&&from=\"xyz\"")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldBeIn, []int{201, 202, 405})
		})

		Convey("Manifests", func() {
			Print("\nManifests")
			// create a blob/layer
			resp, err := resty.R().Post(baseURL + "/v2/repo7/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
			loc := Location(baseURL, resp, config)
			So(loc, ShouldNotBeEmpty)

			resp, err = resty.R().Get(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 204)
			content := []byte("this is a blob")
			digest := godigest.FromBytes(content)
			So(digest, ShouldNotBeNil)
			// monolithic blob upload: success
			resp, err = resty.R().SetQueryParam("digest", digest.String()).
				SetHeader("Content-Type", "application/octet-stream").SetBody(content).Put(loc)
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 201)
			blobLoc := resp.Header().Get("Location")
			So(blobLoc, ShouldNotBeEmpty)
			So(resp.Header().Get("Content-Length"), ShouldEqual, "0")
			So(resp.Header().Get(api.DistContentDigestKey), ShouldNotBeEmpty)

			// create a manifest
			m := ispec.Manifest{Layers: []ispec.Descriptor{{Digest: digest}}}
			content, err = json.Marshal(m)
			So(err, ShouldBeNil)
			digest = godigest.FromBytes(content)
			So(digest, ShouldNotBeNil)
			resp, err = resty.R().SetHeader("Content-Type", "application/vnd.oci.image.manifest.v1+json").
				SetBody(content).Put(baseURL + "/v2/repo7/manifests/test:1.0")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 201)
			d := resp.Header().Get(api.DistContentDigestKey)
			So(d, ShouldNotBeEmpty)
			So(d, ShouldEqual, digest.String())

			// check/get by tag
			resp, err = resty.R().Head(baseURL + "/v2/repo7/manifests/test:1.0")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
			resp, err = resty.R().Get(baseURL + "/v2/repo7/manifests/test:1.0")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
			So(resp.Body(), ShouldNotBeEmpty)
			// check/get by reference
			resp, err = resty.R().Head(baseURL + "/v2/repo7/manifests/" + digest.String())
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
			resp, err = resty.R().Get(baseURL + "/v2/repo7/manifests/" + digest.String())
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
			So(resp.Body(), ShouldNotBeEmpty)

			// delete manifest
			resp, err = resty.R().Delete(baseURL + "/v2/repo7/manifests/test:1.0")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 200)
			// delete again should fail
			resp, err = resty.R().Delete(baseURL + "/v2/repo7/manifests/" + digest.String())
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 404)

			// check/get by tag
			resp, err = resty.R().Head(baseURL + "/v2/repo7/manifests/test:1.0")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 404)
			resp, err = resty.R().Get(baseURL + "/v2/repo7/manifests/test:1.0")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 404)
			So(resp.Body(), ShouldNotBeEmpty)
			// check/get by reference
			resp, err = resty.R().Head(baseURL + "/v2/repo7/manifests/" + digest.String())
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 404)
			resp, err = resty.R().Get(baseURL + "/v2/repo7/manifests/" + digest.String())
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 404)
			So(resp.Body(), ShouldNotBeEmpty)
		})

		// this is an additional test for repository names (alphanumeric)
		Convey("Repository names", func() {
			Print("\nRepository names")
			// create a blob/layer
			resp, err := resty.R().Post(baseURL + "/v2/repotest/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
			resp, err = resty.R().Post(baseURL + "/v2/repotest123/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
			resp, err = resty.R().Post(baseURL + "/v2/repoTest123/blobs/uploads/")
			So(err, ShouldBeNil)
			So(resp.StatusCode(), ShouldEqual, 202)
		})
	})
}

var (
	old  *os.File
	r    *os.File
	w    *os.File
	outC chan string
)

func outputJSONEnter() {
	// this env var instructs goconvey to output results to JSON (stdout)
	os.Setenv("GOCONVEY_REPORTER", "json")

	// stdout capture copied from: https://stackoverflow.com/a/29339052
	old = os.Stdout
	// keep backup of the real stdout
	r, w, _ = os.Pipe()
	outC = make(chan string)
	os.Stdout = w

	// copy the output in a separate goroutine so printing can't block indefinitely
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outC <- buf.String()
	}()
}

func outputJSONExit() {
	// back to normal state
	w.Close()
	os.Stdout = old // restoring the real stdout
	out := <-outC

	// The output of JSON is combined with regular output, so we look for the
	// first occurrence of the "{" character and take everything after that
	rawJSON := "[{" + strings.Join(strings.Split(out, "{")[1:], "{")
	rawJSON = strings.Replace(rawJSON, reporting.OpenJson, "", 1)
	rawJSON = strings.Replace(rawJSON, reporting.CloseJson, "", 1)
	tmp := strings.Split(rawJSON, ",")
	rawJSON = strings.Join(tmp[0:len(tmp)-1], ",") + "]"

	rawJSONMinified := validateMinifyRawJSON(rawJSON)
	fmt.Println(rawJSONMinified)
}

func validateMinifyRawJSON(rawJSON string) string {
	var j interface{}
	err := json.Unmarshal([]byte(rawJSON), &j)
	if err != nil {
		panic(err)
	}
	rawJSONBytesMinified, err := json.Marshal(j)
	if err != nil {
		panic(err)
	}
	return string(rawJSONBytesMinified)
}
