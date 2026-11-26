// Package azblob adapts Azure Blob Storage containers to the domain.Endpoint
// port used by data transfers. Azure is a transfer source only: the write
// operations answer NotImplemented (validation rejects Azure destinations
// before a transfer starts).
package azblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// Opener opens Azure Blob Storage transfer endpoints.
type Opener struct {
	httpClient *http.Client
}

// New returns an Opener; httpClient may be nil for a default client.
func New(httpClient *http.Client) *Opener { return &Opener{httpClient: httpClient} }

// Open returns the endpoint of an Azure Blob Storage side.
func (o *Opener) Open(_ context.Context, side domain.Side) (domain.Endpoint, error) {
	if side.Kind != domain.ProviderAzureBlob {
		return nil, fmt.Errorf("azblob: unsupported provider %s", side.Kind)
	}
	opts := &azblob.ClientOptions{}
	if o.httpClient != nil {
		opts.Transport = o.httpClient
	}
	opts.Retry = policy.RetryOptions{MaxRetries: 2}
	var (
		client *azblob.Client
		err    error
	)
	if side.Creds.Anonymous {
		client, err = azblob.NewClientWithNoCredential(side.Endpoint, opts)
	} else {
		var cred *azblob.SharedKeyCredential
		cred, err = azblob.NewSharedKeyCredential(side.Creds.AzureAccount, side.Creds.AzureKey)
		if err != nil {
			return nil, &domain.EndpointError{Code: "AuthenticationFailed", Message: err.Error()}
		}
		client, err = azblob.NewClientWithSharedKeyCredential(side.Endpoint, cred, opts)
	}
	if err != nil {
		return nil, &domain.EndpointError{Code: "InvalidUri", Message: err.Error()}
	}
	return &endpoint{client: client, container: side.Bucket}, nil
}

type endpoint struct {
	client    *azblob.Client
	container string
}

func (e *endpoint) List(ctx context.Context, prefix string) iter.Seq2[domain.ObjectMeta, error] {
	return func(yield func(domain.ObjectMeta, error) bool) {
		opts := &azblob.ListBlobsFlatOptions{Include: container.ListBlobsInclude{Metadata: true}}
		if prefix != "" {
			opts.Prefix = &prefix
		}
		pager := e.client.NewListBlobsFlatPager(e.container, opts)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				yield(domain.ObjectMeta{}, mapErr(ctx, err))
				return
			}
			for _, item := range blobItems(page) {
				m := blobMeta(item)
				if !yield(m, nil) {
					return
				}
			}
		}
	}
}

// blobItems returns the named blobs of a listing page.
func blobItems(page azblob.ListBlobsFlatResponse) []*container.BlobItem {
	if page.Segment == nil {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(page.Segment.BlobItems), func(i *container.BlobItem) bool { return i == nil || i.Name == nil })
}

func blobMeta(item *container.BlobItem) domain.ObjectMeta {
	m := domain.ObjectMeta{Key: *item.Name, Metadata: lowerKeys(item.Metadata)}
	if p := item.Properties; p != nil {
		m.Size = deref(p.ContentLength)
		m.ETag = etag(p.ETag)
		m.LastModified = deref(p.LastModified)
		m.ContentType = deref(p.ContentType)
		m.CacheControl = deref(p.CacheControl)
		m.ContentDisposition = deref(p.ContentDisposition)
		m.ContentEncoding = deref(p.ContentEncoding)
		m.ContentLanguage = deref(p.ContentLanguage)
	}
	return m
}

func (e *endpoint) Head(ctx context.Context, key string) (domain.ObjectMeta, error) {
	resp, err := e.client.ServiceClient().NewContainerClient(e.container).NewBlobClient(key).GetProperties(ctx, nil)
	if err != nil {
		return domain.ObjectMeta{}, mapErr(ctx, err)
	}
	return domain.ObjectMeta{
		Key:                key,
		Size:               deref(resp.ContentLength),
		ETag:               etag(resp.ETag),
		LastModified:       deref(resp.LastModified),
		ContentType:        deref(resp.ContentType),
		CacheControl:       deref(resp.CacheControl),
		ContentDisposition: deref(resp.ContentDisposition),
		ContentEncoding:    deref(resp.ContentEncoding),
		ContentLanguage:    deref(resp.ContentLanguage),
		Metadata:           lowerKeys(resp.Metadata),
	}, nil
}

func (e *endpoint) Get(ctx context.Context, key string) (io.ReadCloser, domain.ObjectMeta, error) {
	resp, err := e.client.DownloadStream(ctx, e.container, key, nil)
	if err != nil {
		return nil, domain.ObjectMeta{}, mapErr(ctx, err)
	}
	return resp.Body, domain.ObjectMeta{
		Key:                key,
		Size:               deref(resp.ContentLength),
		ETag:               etag(resp.ETag),
		LastModified:       deref(resp.LastModified),
		ContentType:        deref(resp.ContentType),
		CacheControl:       deref(resp.CacheControl),
		ContentDisposition: deref(resp.ContentDisposition),
		ContentEncoding:    deref(resp.ContentEncoding),
		ContentLanguage:    deref(resp.ContentLanguage),
		Metadata:           lowerKeys(resp.Metadata),
	}, nil
}

func (*endpoint) Put(context.Context, string, io.Reader, domain.ObjectMeta) (string, error) {
	return "", errNotImplemented()
}

func (*endpoint) ReplaceMetadata(context.Context, string, domain.ObjectMeta) (string, error) {
	return "", errNotImplemented()
}

func (*endpoint) Delete(context.Context, string) error { return errNotImplemented() }

func errNotImplemented() error {
	return &domain.EndpointError{Code: "NotImplemented", Message: "Azure Blob Storage is only supported as a transfer source"}
}

// mapErr turns an SDK error into the endpoint contract: a cancelled ctx stays
// the ctx error, 404 is errs.NotFound, an Azure error response an
// EndpointError carrying its ErrorCode, anything else a transport
// "RequestError".
func mapErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if re, ok := errors.AsType[*azcore.ResponseError](err); ok {
		if re.StatusCode == http.StatusNotFound && (re.ErrorCode == "BlobNotFound" || re.ErrorCode == "") {
			return errs.NotFound("blob")
		}
		code := re.ErrorCode
		if code == "" {
			code = http.StatusText(re.StatusCode)
		}
		return &domain.EndpointError{Code: code, Message: firstLine(re.Error())}
	}
	return &domain.EndpointError{Code: "RequestError", Message: err.Error()}
}

// firstLine keeps the request summary of an Azure error out of the message.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func etag(e *azcore.ETag) string {
	if e == nil {
		return ""
	}
	return strings.Trim(string(*e), `"`)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func lowerKeys(m map[string]*string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = deref(v)
	}
	return out
}
