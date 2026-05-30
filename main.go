// Command ecr-prime triggers Amazon ECR "create-on-push" for a (possibly
// not-yet-existing) repository by issuing the one write that standard OCI
// clients never do first: POST /v2/<repo>/blobs/uploads/.
//
// Why this exists: cross-account, a repo that doesn't exist yet has no
// repository policy, so any READ (the manifest/blob HEAD that oras and docker
// issue before pushing) returns 403 and the client aborts before any write.
// ECR create-on-push only fires on a WRITE, and only a push-triggered create
// applies the repository-creation-template policy (an explicit CreateRepository
// API call does NOT). So we open — then immediately cancel — a blob upload:
// that write makes ECR create the repo and stamp the template policy, after
// which a normal `oras`/`docker push` succeeds (its HEAD now returns 404 and it
// proceeds).
//
// Runtime deps: none beyond this static binary. Credentials come from the AWS
// SDK default chain (IRSA / Pod Identity / env / profile).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Build-time vars (injected via -ldflags -X).
var (
	Version        = "v0.0.0"
	CommitHash     = "0000000"
	BuildTimestamp = "1970-01-01T00:00:00Z"
	Builder        = "unknown"
	ProjectURL     = "https://github.com/babs/ecr-prime"
)

const (
	exitOK    = 0
	exitErr   = 1 // permanent failure (denied / not configured / bad input result)
	exitUsage = 2
	exitRetry = 3 // transient failure persisted after retries — safe to retry the job

	primeMaxAttempts = 4
	primeBaseDelay   = 500 * time.Millisecond
)

// errTransient marks a failure that survived retries and is worth retrying the
// whole job later (throttling / 5xx / network), as opposed to a permanent deny.
var errTransient = errors.New("transient failure")

var (
	accountRe = regexp.MustCompile(`^[0-9]{12}$`)
	// The only hosts we will ever send the ECR token to.
	hostRe = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr(-fips)?\.[a-z0-9-]+\.amazonaws\.com(\.cn)?$`)
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		account   = flag.String("account", "", "registry AWS account ID (12 digits); when set, pass only <repo>")
		region    = flag.String("region", os.Getenv("AWS_REGION"), "AWS region (required with -account; else parsed from the host, else $AWS_REGION)")
		fips      = flag.Bool("fips", false, "use the ecr-fips endpoint")
		partition = flag.String("partition", "aws", "AWS partition: aws | aws-cn | aws-us-gov")
		keep      = flag.Bool("keep", false, "keep the throwaway upload session instead of cancelling it")
		timeout   = flag.Duration("timeout", 30*time.Second, "overall HTTP timeout")
		logLevel  = flag.String("log-level", getEnv("LOG_LEVEL", "info"), "log level (debug, info, warn, error)")
		showVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Printf("ecr-prime %s (commit %s, built %s by %s)\n", Version, CommitHash, BuildTimestamp, Builder)
		return exitOK
	}

	logger, err := newLogger(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logger init:", err)
		return exitErr
	}
	defer func() { _ = logger.Sync() }()

	if *timeout <= 0 {
		logger.Error("usage_error", zap.Error(fmt.Errorf("-timeout must be > 0")))
		return exitUsage
	}

	host, repo, reg, err := resolveTarget(*account, *region, *partition, *fips, flag.Args())
	if err != nil {
		logger.Error("usage_error", zap.Error(err))
		usage()
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := prime(ctx, logger, reg, host, repo, *keep); err != nil {
		logger.Error("prime_failed", zap.String("registry", host), zap.String("repo", repo), zap.Error(err))
		if errors.Is(err, errTransient) {
			return exitRetry
		}
		return exitErr
	}
	return exitOK
}

// resolveTarget turns flags+args into a validated (host, repo, region). Either
// -account (+ -region) is given and the host is constructed, or a full registry
// host is passed positionally; either way the host must match hostRe so the
// token can only ever go to a real ECR endpoint.
func resolveTarget(account, region, partition string, fips bool, args []string) (host, repo, reg string, err error) {
	if account != "" {
		if !accountRe.MatchString(account) {
			return "", "", "", fmt.Errorf("invalid -account %q (want 12 digits)", account)
		}
		if region == "" {
			return "", "", "", fmt.Errorf("-region is required with -account")
		}
		if len(args) != 1 {
			return "", "", "", fmt.Errorf("with -account, pass exactly one <repo> argument, got %d", len(args))
		}
		repo = cleanRepo(args[0])
		host, err = buildHost(account, region, partition, fips)
		if err != nil {
			return "", "", "", err
		}
		reg = region
	} else {
		host, repo, err = parseTarget(args)
		if err != nil {
			return "", "", "", err
		}
		reg = region
		if reg == "" {
			reg = regionFromHost(host)
		}
	}

	if repo == "" {
		return "", "", "", fmt.Errorf("empty repo")
	}
	if !hostRe.MatchString(host) {
		return "", "", "", fmt.Errorf("%q is not an ECR registry host", host)
	}
	if reg == "" {
		return "", "", "", fmt.Errorf("cannot determine region: pass -region or set $AWS_REGION")
	}
	return host, repo, reg, nil
}

// buildHost assembles an ECR registry host from its parts.
func buildHost(account, region, partition string, fips bool) (string, error) {
	svc := "ecr"
	if fips {
		svc = "ecr-fips"
	}
	var suffix string
	switch partition {
	case "", "aws", "aws-us-gov":
		suffix = "amazonaws.com"
	case "aws-cn":
		suffix = "amazonaws.com.cn"
	default:
		return "", fmt.Errorf("unknown -partition %q (want aws, aws-cn, or aws-us-gov)", partition)
	}
	return fmt.Sprintf("%s.dkr.%s.%s.%s", account, svc, region, suffix), nil
}

// prime obtains an ECR token from the SDK default chain and opens (then cancels)
// a blob upload to trigger create-on-push.
func prime(ctx context.Context, logger *zap.Logger, region, host, repo string, keep bool) error {
	// The SDK already retries throttling/5xx on GetAuthorizationToken; widen it.
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithRetryMaxAttempts(primeMaxAttempts),
	)
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}
	tok, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		// GetAuthorizationToken throttling/5xx (already retried) is transient.
		return fmt.Errorf("%w: get ecr authorization token: %v", errTransient, err)
	}
	if len(tok.AuthorizationData) == 0 || tok.AuthorizationData[0].AuthorizationToken == nil {
		return fmt.Errorf("empty ecr authorization token")
	}
	// AuthorizationToken is already base64("AWS:<password>") => use as Basic auth verbatim.
	auth := "Basic " + *tok.AuthorizationData[0].AuthorizationToken

	client := &http.Client{Timeout: 0} // bounded by ctx
	uploadsURL := fmt.Sprintf("https://%s/v2/%s/blobs/uploads/", host, repo)

	loc, err := doPrimeRequest(ctx, logger, client, uploadsURL, auth, primeMaxAttempts, primeBaseDelay)
	if err != nil {
		return err
	}

	logger.Info("primed", zap.String("registry", host), zap.String("repo", repo))

	if keep {
		return nil
	}
	cl := cancelLocation(host, loc)
	if cl == "" {
		logger.Warn("no_location_header", zap.String("repo", repo)) // upload will auto-expire anyway
		return nil
	}
	if err := cancelUpload(ctx, client, auth, cl); err != nil {
		logger.Warn("cancel_failed", zap.String("repo", repo), zap.Error(err)) // harmless: ECR expires incomplete uploads
	}
	return nil
}

// isRetryableStatus reports whether an HTTP status warrants a retry.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// doPrimeRequest POSTs the blob-upload (create-on-push trigger), retrying
// network errors and 429/5xx with exponential backoff + full jitter, and
// returning the Location header on the 202. A 4xx (e.g. 403 deny) is permanent
// and returns immediately with ECR's JSON error body. The decoupled
// (client, url, auth) signature is the seam unit tests drive via httptest.
func doPrimeRequest(ctx context.Context, logger *zap.Logger, client *http.Client, uploadsURL, auth string, maxAttempts int, base time.Duration) (string, error) {
	var last string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadsURL, nil)
		if err != nil {
			return "", fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", auth)

		resp, err := client.Do(req)
		if err != nil { // transport error — retryable
			last = err.Error()
			if attempt < maxAttempts {
				logger.Warn("retrying", zap.Int("attempt", attempt), zap.Error(err))
				if werr := waitBackoff(ctx, attempt, base); werr != nil {
					return "", fmt.Errorf("%w: post blob upload: %v", errTransient, werr)
				}
				continue
			}
			return "", fmt.Errorf("%w: post blob upload after %d attempts: %s", errTransient, maxAttempts, last)
		}

		code := resp.StatusCode
		locHdr := resp.Header.Get("Location")
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = "(empty body)"
		}

		switch {
		case code == http.StatusAccepted: // 202 == create-on-push fired
			return locHdr, nil
		case isRetryableStatus(code):
			last = fmt.Sprintf("status %d: %s", code, detail)
			if attempt < maxAttempts {
				logger.Warn("retrying", zap.Int("attempt", attempt), zap.Int("status", code))
				if werr := waitBackoff(ctx, attempt, base); werr != nil {
					return "", fmt.Errorf("%w: %s; %v", errTransient, last, werr)
				}
				continue
			}
			return "", fmt.Errorf("%w: %s after %d attempts", errTransient, last, maxAttempts)
		default: // permanent (denied / no template / bad request)
			return "", fmt.Errorf("unexpected status %d (denied / no CREATE_ON_PUSH template / out of scope): %s", code, detail)
		}
	}
	return "", fmt.Errorf("%w: exhausted retries", errTransient) // unreachable
}

// waitBackoff sleeps with exponential backoff + full jitter (random in
// [0, min(cap, base*2^(attempt-1))]), aborting early if ctx is cancelled.
func waitBackoff(ctx context.Context, attempt int, base time.Duration) error {
	const maxDelay = 5 * time.Second
	d := base << (attempt - 1)
	if d > maxDelay || d <= 0 {
		d = maxDelay
	}
	if d > 0 {
		d = time.Duration(rand.Int63n(int64(d) + 1)) // #nosec G404 -- jitter, not security-sensitive
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func cancelUpload(ctx context.Context, client *http.Client, auth, loc string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, loc, nil)
	if err != nil {
		return fmt.Errorf("build delete: %w", err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("delete upload: %w", err)
	}
	_ = resp.Body.Close()
	return nil
}

// parseTarget accepts either "<host> <repo>" or a single "<host>/<repo>[:tag]".
func parseTarget(args []string) (host, repo string, err error) {
	switch len(args) {
	case 2:
		host, repo = strings.TrimSpace(args[0]), args[1]
	case 1:
		i := strings.IndexByte(args[0], '/')
		if i <= 0 {
			return "", "", fmt.Errorf("expected <host>/<repo>, got %q", args[0])
		}
		host, repo = strings.TrimSpace(args[0][:i]), args[0][i+1:]
	default:
		return "", "", fmt.Errorf("expected 1 or 2 arguments (or -account with <repo>), got %d", len(args))
	}
	repo = cleanRepo(repo)
	if host == "" || repo == "" {
		return "", "", fmt.Errorf("empty host or repo")
	}
	return host, repo, nil
}

// cleanRepo trims surrounding space/slashes and strips a trailing :tag (only when
// the colon is in the final path segment, so multi-slash repos survive).
func cleanRepo(s string) string {
	s = strings.TrimSpace(s)
	if j := strings.LastIndexByte(s, ':'); j > strings.LastIndexByte(s, '/') {
		s = s[:j]
	}
	return strings.Trim(s, "/")
}

// regionFromHost extracts <region> from <acct>.dkr.ecr[-fips].<region>.<suffix>.
func regionFromHost(host string) string {
	p := strings.Split(host, ".")
	for i, seg := range p {
		if (seg == "ecr" || seg == "ecr-fips") && i+1 < len(p) {
			return p[i+1]
		}
	}
	return ""
}

// cancelLocation builds the upload-cancel URL pinned to the validated registry
// host: only the path+query of the server-supplied Location is used, so the ECR
// token is never sent to a host other than the one we authenticated against.
func cancelLocation(host, loc string) string {
	if loc == "" {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	return "https://" + host + u.RequestURI()
}

func newLogger(level string) (*zap.Logger, error) {
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, fmt.Errorf("parse log level %q: %w", level, err)
	}
	var cfg zap.Config
	if isTTY(os.Stderr) {
		cfg = zap.NewDevelopmentConfig()
	} else {
		cfg = zap.NewProductionConfig()
	}
	cfg.Level = zap.NewAtomicLevelAt(lvl)
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.DisableStacktrace = true // a CLI's error paths are expected; stacktraces are noise
	return cfg.Build()
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func usage() {
	fmt.Fprintf(os.Stderr, `ecr-prime — trigger ECR create-on-push (write-first), then cancel the upload.

Usage:
  ecr-prime --account <id> --region <region> [flags] <repo>[:tag]
  ecr-prime [flags] <registry-host> <repo>
  ecr-prime [flags] <registry-host>/<repo>[:tag]

Examples:
  ecr-prime --account 123456789012 --region eu-west-1 my-namespace/my-app
  ecr-prime 123456789012.dkr.ecr.eu-west-1.amazonaws.com my-namespace/my-app

Exit: 0 primed (repo created), 1 denied/permanent error, 2 usage,
      3 transient failure after retries (safe to retry the job).

Flags:
`)
	flag.PrintDefaults()
}
