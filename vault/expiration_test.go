package vault

import (
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault/helper/logformat"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
	"github.com/hashicorp/vault/physical"
	log "github.com/mgutz/logxi/v1"
)

var (
	testImagePull sync.Once
)

// mockExpiration returns a mock expiration manager
func mockExpiration(t testing.TB) *ExpirationManager {
	_, ts, _, _ := TestCoreWithTokenStore(t)
	return ts.expiration
}

func mockBackendExpiration(t testing.TB, backend physical.Backend) (*Core, *ExpirationManager) {
	c, ts, _, _ := TestCoreWithBackendTokenStore(t, backend)
	return c, ts.expiration
}

func BenchmarkExpiration_Restore_Postgres(b *testing.B) {
	addr := os.Getenv("PHYSICAL_BACKEND_BENCHMARK_ADDR")
	if addr == "" {
		b.Fatal("PHYSICAL_BACKEND_BENCHMARK_ADDR not set")
	}

	logger := logformat.NewVaultLogger(log.LevelTrace)
	physicalBackend, err := physical.NewBackend("postgresql", logger, map[string]string{
		"connection_url": addr,
	})
	if err != nil {
		b.Fatalf("err: %s", err)
	}

	benchmarkExpirationBackend(b, physicalBackend, 1000) // 1,000 leases
}

func BenchmarkExpiration_Restore_Etcd(b *testing.B) {
	addr := os.Getenv("PHYSICAL_BACKEND_BENCHMARK_ADDR")
	randPath := fmt.Sprintf("vault-%d/", time.Now().Unix())

	logger := logformat.NewVaultLogger(log.LevelTrace)
	physicalBackend, err := physical.NewBackend("etcd", logger, map[string]string{
		"address":      addr,
		"path":         randPath,
		"max_parallel": "256",
	})
	if err != nil {
		b.Fatalf("err: %s", err)
	}

	benchmarkExpirationBackend(b, physicalBackend, 10000) // 10,000 leases
}

func BenchmarkExpiration_Restore_Consul(b *testing.B) {
	addr := os.Getenv("PHYSICAL_BACKEND_BENCHMARK_ADDR")
	randPath := fmt.Sprintf("vault-%d/", time.Now().Unix())

	logger := logformat.NewVaultLogger(log.LevelTrace)
	physicalBackend, err := physical.NewBackend("consul", logger, map[string]string{
		"address":      addr,
		"path":         randPath,
		"max_parallel": "256",
	})
	if err != nil {
		b.Fatalf("err: %s", err)
	}

	benchmarkExpirationBackend(b, physicalBackend, 10000) // 10,000 leases
}

func BenchmarkExpiration_Restore_S3(b *testing.B) {

	endpoint := os.Getenv("AWS_S3_ENDPOINT")

	region := os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region = "us-east-1"
	}

	s3conn := s3.New(session.New(&aws.Config{
		Credentials: credentials.NewEnvCredentials(),
		Endpoint:    aws.String(endpoint),
		Region:      aws.String(region),
	}))

	var randInt = rand.New(rand.NewSource(time.Now().UnixNano())).Int()
	bucket := fmt.Sprintf("vault-s3-testacc-%d", randInt)

	_, err := s3conn.CreateBucket(&s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		b.Fatalf("unable to create test bucket: %s", err)
	}

	defer func() {
		// Gotta list all the objects and delete them
		// before being able to delete the bucket
		listResp, _ := s3conn.ListObjects(&s3.ListObjectsInput{
			Bucket: aws.String(bucket),
		})

		objects := &s3.Delete{}
		for _, key := range listResp.Contents {
			oi := &s3.ObjectIdentifier{Key: key.Key}
			objects.Objects = append(objects.Objects, oi)
		}

		s3conn.DeleteObjects(&s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: objects,
		})

		_, err := s3conn.DeleteBucket(&s3.DeleteBucketInput{Bucket: aws.String(bucket)})
		if err != nil {
			b.Fatalf("err: %s", err)
		}
	}()

	logger := logformat.NewVaultLogger(log.LevelTrace)
	physicalBackend, err := physical.NewBackend("s3", logger, map[string]string{
		"bucket": bucket,
	})
	if err != nil {
		b.Fatal(err)
	}

	benchmarkExpirationBackend(b, physicalBackend, 1000) // 1,000 Leases
}

func BenchmarkExpiration_Restore_InMem(b *testing.B) {
	logger := logformat.NewVaultLogger(log.LevelTrace)
	benchmarkExpirationBackend(b, physical.NewInmem(logger), 100000) // 100,000 Leases
}

func benchmarkExpirationBackend(b *testing.B, physicalBackend physical.Backend, numLeases int) {
	c, exp := mockBackendExpiration(b, physicalBackend)
	noop := &NoopBackend{}
	view := NewBarrierView(c.barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		b.Fatal(err)
	}
	exp.router.Mount(noop, "prod/aws/", &MountEntry{UUID: meUUID}, view)

	// Register fake leases
	for i := 0; i < numLeases; i++ {
		pathUUID, err := uuid.GenerateUUID()
		if err != nil {
			b.Fatal(err)
		}

		req := &logical.Request{
			Operation: logical.ReadOperation,
			Path:      "prod/aws/" + pathUUID,
		}
		resp := &logical.Response{
			Secret: &logical.Secret{
				LeaseOptions: logical.LeaseOptions{
					TTL: 400 * time.Second,
				},
			},
			Data: map[string]interface{}{
				"access_key": "xyz",
				"secret_key": "abcd",
			},
		}
		_, err = exp.Register(req, resp)
		if err != nil {
			b.Fatalf("err: %v", err)
		}
	}

	// Stop everything
	err = exp.Stop()
	if err != nil {
		b.Fatalf("err: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err = exp.Restore()
		// Restore
		if err != nil {
			b.Fatalf("err: %v", err)
		}
	}
	b.StopTimer()
}

func TestExpiration_Restore(t *testing.T) {
	exp := mockExpiration(t)
	noop := &NoopBackend{}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "prod/aws/", &MountEntry{UUID: meUUID}, view)

	paths := []string{
		"prod/aws/foo",
		"prod/aws/sub/bar",
		"prod/aws/zip",
	}
	for _, path := range paths {
		req := &logical.Request{
			Operation: logical.ReadOperation,
			Path:      path,
		}
		resp := &logical.Response{
			Secret: &logical.Secret{
				LeaseOptions: logical.LeaseOptions{
					TTL: 20 * time.Millisecond,
				},
			},
			Data: map[string]interface{}{
				"access_key": "xyz",
				"secret_key": "abcd",
			},
		}
		_, err := exp.Register(req, resp)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Stop everything
	err = exp.Stop()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Restore
	err = exp.Restore()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure all are reaped
	start := time.Now()
	for time.Now().Sub(start) < time.Second {
		noop.Lock()
		less := len(noop.Requests) < 3
		noop.Unlock()

		if less {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		break
	}
	for _, req := range noop.Requests {
		if req.Operation != logical.RevokeOperation {
			t.Fatalf("Bad: %v", req)
		}
	}
}

func TestExpiration_Register(t *testing.T) {
	exp := mockExpiration(t)
	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "prod/aws/foo",
	}
	resp := &logical.Response{
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL: time.Hour,
			},
		},
		Data: map[string]interface{}{
			"access_key": "xyz",
			"secret_key": "abcd",
		},
	}

	id, err := exp.Register(req, resp)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if !strings.HasPrefix(id, req.Path) {
		t.Fatalf("bad: %s", id)
	}

	if len(id) <= len(req.Path) {
		t.Fatalf("bad: %s", id)
	}
}

func TestExpiration_RegisterAuth(t *testing.T) {
	exp := mockExpiration(t)
	root, err := exp.tokenStore.rootToken()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	auth := &logical.Auth{
		ClientToken: root.ID,
		LeaseOptions: logical.LeaseOptions{
			TTL: time.Hour,
		},
	}

	err = exp.RegisterAuth("auth/github/login", auth)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestExpiration_RegisterAuth_NoLease(t *testing.T) {
	exp := mockExpiration(t)
	root, err := exp.tokenStore.rootToken()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	auth := &logical.Auth{
		ClientToken: root.ID,
	}

	err = exp.RegisterAuth("auth/github/login", auth)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Should not be able to renew, no expiration
	resp, err := exp.RenewToken(&logical.Request{}, "auth/github/login", root.ID, 0)
	if err != nil && (err != logical.ErrInvalidRequest || (resp != nil && resp.IsError() && resp.Error().Error() != "lease not found or lease is not renewable")) {
		t.Fatalf("bad: err:%v resp:%#v", err, resp)
	}
	if resp == nil {
		t.Fatal("expected a response")
	}

	// Wait and check token is not invalidated
	time.Sleep(20 * time.Millisecond)

	// Verify token does not get revoked
	out, err := exp.tokenStore.Lookup(root.ID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out == nil {
		t.Fatalf("missing token")
	}
}

func TestExpiration_Revoke(t *testing.T) {
	exp := mockExpiration(t)
	noop := &NoopBackend{}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "prod/aws/", &MountEntry{UUID: meUUID}, view)

	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "prod/aws/foo",
	}
	resp := &logical.Response{
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL: time.Hour,
			},
		},
		Data: map[string]interface{}{
			"access_key": "xyz",
			"secret_key": "abcd",
		},
	}

	id, err := exp.Register(req, resp)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if err := exp.Revoke(id); err != nil {
		t.Fatalf("err: %v", err)
	}

	req = noop.Requests[0]
	if req.Operation != logical.RevokeOperation {
		t.Fatalf("Bad: %v", req)
	}
}

func TestExpiration_RevokeOnExpire(t *testing.T) {
	exp := mockExpiration(t)
	noop := &NoopBackend{}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "prod/aws/", &MountEntry{UUID: meUUID}, view)

	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "prod/aws/foo",
	}
	resp := &logical.Response{
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL: 20 * time.Millisecond,
			},
		},
		Data: map[string]interface{}{
			"access_key": "xyz",
			"secret_key": "abcd",
		},
	}

	_, err = exp.Register(req, resp)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	start := time.Now()
	for time.Now().Sub(start) < time.Second {
		req = nil

		noop.Lock()
		if len(noop.Requests) > 0 {
			req = noop.Requests[0]
		}
		noop.Unlock()
		if req == nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if req.Operation != logical.RevokeOperation {
			t.Fatalf("Bad: %v", req)
		}

		break
	}
}

func TestExpiration_RevokePrefix(t *testing.T) {
	exp := mockExpiration(t)
	noop := &NoopBackend{}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "prod/aws/", &MountEntry{UUID: meUUID}, view)

	paths := []string{
		"prod/aws/foo",
		"prod/aws/sub/bar",
		"prod/aws/zip",
	}
	for _, path := range paths {
		req := &logical.Request{
			Operation: logical.ReadOperation,
			Path:      path,
		}
		resp := &logical.Response{
			Secret: &logical.Secret{
				LeaseOptions: logical.LeaseOptions{
					TTL: 20 * time.Millisecond,
				},
			},
			Data: map[string]interface{}{
				"access_key": "xyz",
				"secret_key": "abcd",
			},
		}
		_, err := exp.Register(req, resp)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Should nuke all the keys
	if err := exp.RevokePrefix("prod/aws/"); err != nil {
		t.Fatalf("err: %v", err)
	}

	if len(noop.Requests) != 3 {
		t.Fatalf("Bad: %v", noop.Requests)
	}
	for _, req := range noop.Requests {
		if req.Operation != logical.RevokeOperation {
			t.Fatalf("Bad: %v", req)
		}
	}

	expect := []string{
		"foo",
		"sub/bar",
		"zip",
	}
	sort.Strings(noop.Paths)
	sort.Strings(expect)
	if !reflect.DeepEqual(noop.Paths, expect) {
		t.Fatalf("bad: %v", noop.Paths)
	}
}

func TestExpiration_RevokeByToken(t *testing.T) {
	exp := mockExpiration(t)
	noop := &NoopBackend{}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "prod/aws/", &MountEntry{UUID: meUUID}, view)

	paths := []string{
		"prod/aws/foo",
		"prod/aws/sub/bar",
		"prod/aws/zip",
	}
	for _, path := range paths {
		req := &logical.Request{
			Operation:   logical.ReadOperation,
			Path:        path,
			ClientToken: "foobarbaz",
		}
		resp := &logical.Response{
			Secret: &logical.Secret{
				LeaseOptions: logical.LeaseOptions{
					TTL: 20 * time.Millisecond,
				},
			},
			Data: map[string]interface{}{
				"access_key": "xyz",
				"secret_key": "abcd",
			},
		}
		_, err := exp.Register(req, resp)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Should nuke all the keys
	te := &TokenEntry{
		ID: "foobarbaz",
	}
	if err := exp.RevokeByToken(te); err != nil {
		t.Fatalf("err: %v", err)
	}

	if len(noop.Requests) != 3 {
		t.Fatalf("Bad: %v", noop.Requests)
	}
	for _, req := range noop.Requests {
		if req.Operation != logical.RevokeOperation {
			t.Fatalf("Bad: %v", req)
		}
	}

	expect := []string{
		"foo",
		"sub/bar",
		"zip",
	}
	sort.Strings(noop.Paths)
	sort.Strings(expect)
	if !reflect.DeepEqual(noop.Paths, expect) {
		t.Fatalf("bad: %v", noop.Paths)
	}
}

func TestExpiration_RenewToken(t *testing.T) {
	exp := mockExpiration(t)
	root, err := exp.tokenStore.rootToken()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Register a token
	auth := &logical.Auth{
		ClientToken: root.ID,
		LeaseOptions: logical.LeaseOptions{
			TTL:       time.Hour,
			Renewable: true,
		},
	}
	err = exp.RegisterAuth("auth/token/login", auth)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Renew the token
	out, err := exp.RenewToken(&logical.Request{}, "auth/token/login", root.ID, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if auth.ClientToken != out.Auth.ClientToken {
		t.Fatalf("Bad: %#v", out)
	}
}

func TestExpiration_RenewToken_NotRenewable(t *testing.T) {
	exp := mockExpiration(t)
	root, err := exp.tokenStore.rootToken()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Register a token
	auth := &logical.Auth{
		ClientToken: root.ID,
		LeaseOptions: logical.LeaseOptions{
			TTL:       time.Hour,
			Renewable: false,
		},
	}
	err = exp.RegisterAuth("auth/github/login", auth)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to renew the token
	resp, err := exp.RenewToken(&logical.Request{}, "auth/github/login", root.ID, 0)
	if err != nil && (err != logical.ErrInvalidRequest || (resp != nil && resp.IsError() && resp.Error().Error() != "lease is not renewable")) {
		t.Fatalf("bad: err:%v resp:%#v", err, resp)
	}
	if resp == nil {
		t.Fatal("expected a response")
	}

}

func TestExpiration_Renew(t *testing.T) {
	exp := mockExpiration(t)
	noop := &NoopBackend{}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "prod/aws/", &MountEntry{UUID: meUUID}, view)

	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "prod/aws/foo",
	}
	resp := &logical.Response{
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL:       20 * time.Millisecond,
				Renewable: true,
			},
		},
		Data: map[string]interface{}{
			"access_key": "xyz",
			"secret_key": "abcd",
		},
	}

	id, err := exp.Register(req, resp)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	noop.Response = &logical.Response{
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL: 20 * time.Millisecond,
			},
		},
		Data: map[string]interface{}{
			"access_key": "123",
			"secret_key": "abcd",
		},
	}

	out, err := exp.Renew(id, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	noop.Lock()
	defer noop.Unlock()

	if !reflect.DeepEqual(out, noop.Response) {
		t.Fatalf("Bad: %#v", out)
	}

	if len(noop.Requests) != 1 {
		t.Fatalf("Bad: %#v", noop.Requests)
	}
	req = noop.Requests[0]
	if req.Operation != logical.RenewOperation {
		t.Fatalf("Bad: %v", req)
	}
}

func TestExpiration_Renew_NotRenewable(t *testing.T) {
	exp := mockExpiration(t)
	noop := &NoopBackend{}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "prod/aws/", &MountEntry{UUID: meUUID}, view)

	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "prod/aws/foo",
	}
	resp := &logical.Response{
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL:       20 * time.Millisecond,
				Renewable: false,
			},
		},
		Data: map[string]interface{}{
			"access_key": "xyz",
			"secret_key": "abcd",
		},
	}

	id, err := exp.Register(req, resp)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	_, err = exp.Renew(id, 0)
	if err.Error() != "lease is not renewable" {
		t.Fatalf("err: %v", err)
	}

	noop.Lock()
	defer noop.Unlock()

	if len(noop.Requests) != 0 {
		t.Fatalf("Bad: %#v", noop.Requests)
	}
}

func TestExpiration_Renew_RevokeOnExpire(t *testing.T) {
	exp := mockExpiration(t)
	noop := &NoopBackend{}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "prod/aws/", &MountEntry{UUID: meUUID}, view)

	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "prod/aws/foo",
	}
	resp := &logical.Response{
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL:       20 * time.Millisecond,
				Renewable: true,
			},
		},
		Data: map[string]interface{}{
			"access_key": "xyz",
			"secret_key": "abcd",
		},
	}

	id, err := exp.Register(req, resp)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	noop.Response = &logical.Response{
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL: 20 * time.Millisecond,
			},
		},
		Data: map[string]interface{}{
			"access_key": "123",
			"secret_key": "abcd",
		},
	}

	_, err = exp.Renew(id, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	start := time.Now()
	for time.Now().Sub(start) < time.Second {
		req = nil

		noop.Lock()
		if len(noop.Requests) >= 2 {
			req = noop.Requests[1]
		}
		noop.Unlock()

		if req == nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if req.Operation != logical.RevokeOperation {
			t.Fatalf("Bad: %v", req)
		}
		break
	}
}

func TestExpiration_revokeEntry(t *testing.T) {
	exp := mockExpiration(t)

	noop := &NoopBackend{}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "", &MountEntry{UUID: meUUID}, view)

	le := &leaseEntry{
		LeaseID: "foo/bar/1234",
		Path:    "foo/bar",
		Data: map[string]interface{}{
			"testing": true,
		},
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL: time.Minute,
			},
		},
		IssueTime:  time.Now(),
		ExpireTime: time.Now(),
	}

	err = exp.revokeEntry(le)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	noop.Lock()
	defer noop.Unlock()

	req := noop.Requests[0]
	if req.Operation != logical.RevokeOperation {
		t.Fatalf("Bad: %v", req)
	}
	if req.Path != le.Path {
		t.Fatalf("Bad: %v", req)
	}
	if !reflect.DeepEqual(req.Data, le.Data) {
		t.Fatalf("Bad: %v", req)
	}
}

func TestExpiration_revokeEntry_token(t *testing.T) {
	exp := mockExpiration(t)
	root, err := exp.tokenStore.rootToken()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// N.B.: Vault doesn't allow both a secret and auth to be returned, but the
	// reason for both is that auth needs to be included in order to use the
	// token store as it's the only mounted backend, *but* RegisterAuth doesn't
	// actually create the index by token, only Register (for a Secret) does.
	// So without the Secret we don't do anything when removing the index which
	// (at the time of writing) now fails because a bug causing every token
	// expiration to do an extra delete to a non-existent key has been fixed,
	// and this test relies on this nonstandard behavior.
	le := &leaseEntry{
		LeaseID: "foo/bar/1234",
		Auth: &logical.Auth{
			ClientToken: root.ID,
			LeaseOptions: logical.LeaseOptions{
				TTL: time.Minute,
			},
		},
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL: time.Minute,
			},
		},
		ClientToken: root.ID,
		Path:        "foo/bar",
		IssueTime:   time.Now(),
		ExpireTime:  time.Now(),
	}

	if err := exp.persistEntry(le); err != nil {
		t.Fatalf("error persisting entry: %v", err)
	}
	if err := exp.createIndexByToken(le.ClientToken, le.LeaseID); err != nil {
		t.Fatalf("error creating secondary index: %v", err)
	}
	exp.updatePending(le, le.Secret.LeaseTotal())

	indexEntry, err := exp.indexByToken(le.ClientToken, le.LeaseID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if indexEntry == nil {
		t.Fatalf("err: should have found a secondary index entry")
	}

	err = exp.revokeEntry(le)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	out, err := exp.tokenStore.Lookup(le.ClientToken)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != nil {
		t.Fatalf("bad: %v", out)
	}

	indexEntry, err = exp.indexByToken(le.ClientToken, le.LeaseID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if indexEntry != nil {
		t.Fatalf("err: should not have found a secondary index entry")
	}
}

func TestExpiration_renewEntry(t *testing.T) {
	exp := mockExpiration(t)

	noop := &NoopBackend{
		Response: &logical.Response{
			Secret: &logical.Secret{
				LeaseOptions: logical.LeaseOptions{
					Renewable: true,
					TTL:       time.Hour,
				},
			},
			Data: map[string]interface{}{
				"testing": false,
			},
		},
	}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "logical/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "", &MountEntry{UUID: meUUID}, view)

	le := &leaseEntry{
		LeaseID: "foo/bar/1234",
		Path:    "foo/bar",
		Data: map[string]interface{}{
			"testing": true,
		},
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL: time.Minute,
			},
		},
		IssueTime:  time.Now(),
		ExpireTime: time.Now(),
	}

	resp, err := exp.renewEntry(le, time.Second)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	noop.Lock()
	defer noop.Unlock()

	if !reflect.DeepEqual(resp, noop.Response) {
		t.Fatalf("bad: %#v", resp)
	}

	req := noop.Requests[0]
	if req.Operation != logical.RenewOperation {
		t.Fatalf("Bad: %v", req)
	}
	if req.Path != le.Path {
		t.Fatalf("Bad: %v", req)
	}
	if !reflect.DeepEqual(req.Data, le.Data) {
		t.Fatalf("Bad: %v", req)
	}
	if req.Secret.Increment != time.Second {
		t.Fatalf("Bad: %v", req)
	}
	if req.Secret.IssueTime.IsZero() {
		t.Fatalf("Bad: %v", req)
	}
}

func TestExpiration_renewAuthEntry(t *testing.T) {
	exp := mockExpiration(t)

	noop := &NoopBackend{
		Response: &logical.Response{
			Auth: &logical.Auth{
				LeaseOptions: logical.LeaseOptions{
					Renewable: true,
					TTL:       time.Hour,
				},
			},
		},
	}
	_, barrier, _ := mockBarrier(t)
	view := NewBarrierView(barrier, "auth/foo/")
	meUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	exp.router.Mount(noop, "auth/foo/", &MountEntry{UUID: meUUID}, view)

	le := &leaseEntry{
		LeaseID: "auth/foo/1234",
		Path:    "auth/foo/login",
		Auth: &logical.Auth{
			LeaseOptions: logical.LeaseOptions{
				Renewable: true,
				TTL:       time.Minute,
			},
			InternalData: map[string]interface{}{
				"MySecret": "secret",
			},
		},
		IssueTime:  time.Now(),
		ExpireTime: time.Now().Add(time.Minute),
	}

	resp, err := exp.renewAuthEntry(&logical.Request{}, le, time.Second)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	noop.Lock()
	defer noop.Unlock()

	if !reflect.DeepEqual(resp, noop.Response) {
		t.Fatalf("bad: %#v", resp)
	}

	req := noop.Requests[0]
	if req.Operation != logical.RenewOperation {
		t.Fatalf("Bad: %v", req)
	}
	if req.Path != "login" {
		t.Fatalf("Bad: %v", req)
	}
	if req.Auth.Increment != time.Second {
		t.Fatalf("Bad: %v", req)
	}
	if req.Auth.IssueTime.IsZero() {
		t.Fatalf("Bad: %v", req)
	}
	if req.Auth.InternalData["MySecret"] != "secret" {
		t.Fatalf("Bad: %v", req)
	}
}

func TestExpiration_PersistLoadDelete(t *testing.T) {
	exp := mockExpiration(t)
	lastTime := time.Now()
	le := &leaseEntry{
		LeaseID: "foo/bar/1234",
		Path:    "foo/bar",
		Data: map[string]interface{}{
			"testing": true,
		},
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL: time.Minute,
			},
		},
		IssueTime:       lastTime,
		ExpireTime:      lastTime,
		LastRenewalTime: lastTime,
	}
	if err := exp.persistEntry(le); err != nil {
		t.Fatalf("err: %v", err)
	}

	out, err := exp.loadEntry("foo/bar/1234")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !le.LastRenewalTime.Equal(out.LastRenewalTime) ||
		!le.IssueTime.Equal(out.IssueTime) ||
		!le.ExpireTime.Equal(out.ExpireTime) {
		t.Fatalf("bad: expected:\n%#v\nactual:\n%#v", le, out)
	}
	le.LastRenewalTime = out.LastRenewalTime
	le.IssueTime = out.IssueTime
	le.ExpireTime = out.ExpireTime
	if !reflect.DeepEqual(out, le) {
		t.Fatalf("bad: expected:\n%#v\nactual:\n%#v", le, out)
	}

	err = exp.deleteEntry("foo/bar/1234")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	out, err = exp.loadEntry("foo/bar/1234")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != nil {
		t.Fatalf("out: %#v", out)
	}
}

func TestLeaseEntry(t *testing.T) {
	le := &leaseEntry{
		LeaseID: "foo/bar/1234",
		Path:    "foo/bar",
		Data: map[string]interface{}{
			"testing": true,
		},
		Secret: &logical.Secret{
			LeaseOptions: logical.LeaseOptions{
				TTL: time.Minute,
			},
		},
		IssueTime:  time.Now(),
		ExpireTime: time.Now(),
	}

	enc, err := le.encode()
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	out, err := decodeLeaseEntry(enc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if !reflect.DeepEqual(out.Data, le.Data) {
		t.Fatalf("got: %#v, expect %#v", out, le)
	}
}

func TestExpiration_RevokeForce(t *testing.T) {
	core, _, _, root := TestCoreWithTokenStore(t)

	core.logicalBackends["badrenew"] = badRenewFactory
	me := &MountEntry{
		Table: mountTableType,
		Path:  "badrenew/",
		Type:  "badrenew",
	}

	err := core.mount(me)
	if err != nil {
		t.Fatal(err)
	}

	req := &logical.Request{
		Operation:   logical.ReadOperation,
		Path:        "badrenew/creds",
		ClientToken: root,
	}

	resp, err := core.HandleRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("response was nil")
	}
	if resp.Secret == nil {
		t.Fatalf("response secret was nil, response was %#v", *resp)
	}

	req.Operation = logical.UpdateOperation
	req.Path = "sys/revoke-prefix/badrenew/creds"

	resp, err = core.HandleRequest(req)
	if err == nil {
		t.Fatal("expected error")
	}

	req.Path = "sys/revoke-force/badrenew/creds"
	resp, err = core.HandleRequest(req)
	if err != nil {
		t.Fatalf("got error: %s", err)
	}
}

func badRenewFactory(conf *logical.BackendConfig) (logical.Backend, error) {
	be := &framework.Backend{
		Paths: []*framework.Path{
			&framework.Path{
				Pattern: "creds",
				Callbacks: map[logical.Operation]framework.OperationFunc{
					logical.ReadOperation: func(*logical.Request, *framework.FieldData) (*logical.Response, error) {
						resp := &logical.Response{
							Secret: &logical.Secret{
								InternalData: map[string]interface{}{
									"secret_type": "badRenewBackend",
								},
							},
						}
						resp.Secret.TTL = time.Second * 30
						return resp, nil
					},
				},
			},
		},

		Secrets: []*framework.Secret{
			&framework.Secret{
				Type: "badRenewBackend",
				Revoke: func(*logical.Request, *framework.FieldData) (*logical.Response, error) {
					return nil, fmt.Errorf("always errors")
				},
			},
		},
	}

	return be.Setup(conf)
}
