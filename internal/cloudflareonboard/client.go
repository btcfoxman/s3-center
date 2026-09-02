package cloudflareonboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const maxCloudflareResponseBytes = 2 << 20

type oauthTokenResponse struct {
	AccessToken      string `json:"access_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type apiEnvelope[T any] struct {
	Success    bool       `json:"success"`
	Result     T          `json:"result"`
	Errors     []apiError `json:"errors"`
	ResultInfo struct {
		Page       int    `json:"page"`
		TotalPages int    `json:"total_pages"`
		Cursor     string `json:"cursor"`
	} `json:"result_info"`
}

type apiAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type apiBucket struct {
	Name         string `json:"name"`
	Jurisdiction string `json:"jurisdiction"`
	Location     string `json:"location"`
	StorageClass string `json:"storage_class"`
}

type permissionGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type createdToken struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

func (s *Service) exchangeCode(ctx context.Context, client *http.Client, code, verifier string) (string, error) {
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.cfg.RedirectURL},
		"code_verifier": {verifier},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return "", fmt.Errorf("create OAuth token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if s.cfg.TokenEndpointAuthMethod == "client_secret_post" {
		values.Set("client_id", s.cfg.ClientID)
		values.Set("client_secret", s.cfg.ClientSecret)
		request.Body = io.NopCloser(strings.NewReader(values.Encode()))
		request.ContentLength = int64(len(values.Encode()))
	} else {
		request.SetBasicAuth(s.cfg.ClientID, s.cfg.ClientSecret)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("exchange Cloudflare OAuth code: %w", err)
	}
	defer response.Body.Close()
	var result oauthTokenResponse
	if err := decodeLimitedJSON(response.Body, &result); err != nil {
		return "", fmt.Errorf("decode Cloudflare OAuth response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || result.AccessToken == "" {
		message := firstNonEmpty(result.ErrorDescription, result.Error)
		if message == "" {
			message = response.Status
		}
		return "", fmt.Errorf("cloudflare OAuth exchange failed: %s", message)
	}
	return result.AccessToken, nil
}

func (s *Service) discoverAccounts(ctx context.Context, client *http.Client, token string) ([]Account, error) {
	accounts, err := s.listAccounts(ctx, client, token)
	if err != nil {
		return nil, err
	}
	result := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		buckets, bucketErr := s.listBuckets(ctx, client, token, account.ID)
		view := Account{ID: strings.ToLower(account.ID), Name: account.Name, Buckets: buckets}
		if bucketErr != nil {
			view.Error = truncate(bucketErr.Error(), 240)
		}
		result = append(result, view)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *Service) listAccounts(ctx context.Context, client *http.Client, token string) ([]apiAccount, error) {
	var all []apiAccount
	for page := 1; page <= 20; page++ {
		path := "/accounts?per_page=50&page=" + strconv.Itoa(page)
		var envelope apiEnvelope[[]apiAccount]
		if err := s.doAPI(ctx, client, http.MethodGet, path, token, nil, &envelope); err != nil {
			return nil, err
		}
		all = append(all, envelope.Result...)
		if envelope.ResultInfo.TotalPages <= page || len(envelope.Result) == 0 {
			break
		}
	}
	return all, nil
}

func (s *Service) listBuckets(ctx context.Context, client *http.Client, token, accountID string) ([]Bucket, error) {
	var result []Bucket
	cursor := ""
	for range 20 {
		path := "/accounts/" + url.PathEscape(accountID) + "/r2/buckets?per_page=1000"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var envelope apiEnvelope[struct {
			Buckets []apiBucket `json:"buckets"`
		}]
		if err := s.doAPI(ctx, client, http.MethodGet, path, token, nil, &envelope); err != nil {
			return nil, err
		}
		for _, bucket := range envelope.Result.Buckets {
			jurisdiction := bucket.Jurisdiction
			if jurisdiction == "" {
				jurisdiction = "default"
			}
			result = append(result, Bucket{
				Name: bucket.Name, Jurisdiction: jurisdiction, Location: bucket.Location, StorageClass: bucket.StorageClass,
			})
		}
		if envelope.ResultInfo.Cursor == "" || envelope.ResultInfo.Cursor == cursor {
			break
		}
		cursor = envelope.ResultInfo.Cursor
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *Service) createBucketToken(
	ctx context.Context,
	client *http.Client,
	oauthToken string,
	account Account,
	bucket Bucket,
) (string, string, error) {
	permissionID, err := s.bucketWritePermissionID(ctx, client, oauthToken, account.ID)
	if err != nil {
		return "", "", err
	}
	policyID, err := randomID("", 16)
	if err != nil {
		return "", "", err
	}
	jurisdiction := firstNonEmpty(bucket.Jurisdiction, "default")
	resource := "com.cloudflare.edge.r2.bucket." + account.ID + "_" + jurisdiction + "_" + bucket.Name
	body := map[string]any{
		"name": fmt.Sprintf("s3-center/%s/%s", truncate(account.Name, 48), truncate(bucket.Name, 48)),
		"policies": []any{map[string]any{
			"id": policyID, "effect": "allow",
			"resources": map[string]string{resource: "*"},
			"permission_groups": []any{map[string]string{
				"id": permissionID, "name": "Workers R2 Storage Bucket Item Write",
			}},
		}},
	}
	var envelope apiEnvelope[createdToken]
	path := "/accounts/" + url.PathEscape(account.ID) + "/tokens"
	if err := s.doAPI(ctx, client, http.MethodPost, path, oauthToken, body, &envelope); err != nil {
		return "", "", err
	}
	if envelope.Result.ID == "" || envelope.Result.Value == "" {
		return "", "", errors.New("cloudflare created an R2 token without returning its one-time value")
	}
	secret := sha256.Sum256([]byte(envelope.Result.Value))
	return envelope.Result.ID, hex.EncodeToString(secret[:]), nil
}

func (s *Service) bucketWritePermissionID(ctx context.Context, client *http.Client, token, accountID string) (string, error) {
	path := "/accounts/" + url.PathEscape(accountID) + "/tokens/permission_groups?" + url.Values{
		"name":  {"Workers R2 Storage Bucket Item Write"},
		"scope": {"com.cloudflare.edge.r2.bucket"},
	}.Encode()
	var envelope apiEnvelope[[]permissionGroup]
	if err := s.doAPI(ctx, client, http.MethodGet, path, token, nil, &envelope); err != nil {
		return "", err
	}
	for _, permission := range envelope.Result {
		if permission.Name == "Workers R2 Storage Bucket Item Write" && permission.ID != "" {
			return permission.ID, nil
		}
	}
	return "", errors.New("cloudflare did not expose the bucket-scoped R2 write permission; verify the OAuth token-management scope")
}

func (s *Service) deleteAccountToken(ctx context.Context, client *http.Client, oauthToken, accountID, tokenID string) error {
	var envelope apiEnvelope[struct {
		ID string `json:"id"`
	}]
	path := "/accounts/" + url.PathEscape(accountID) + "/tokens/" + url.PathEscape(tokenID)
	return s.doAPI(ctx, client, http.MethodDelete, path, oauthToken, nil, &envelope)
}

func (s *Service) doAPI(
	ctx context.Context,
	client *http.Client,
	method, path, token string,
	body any,
	out any,
) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Cloudflare API request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, s.apiBaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("create Cloudflare API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("call Cloudflare API: %w", err)
	}
	defer response.Body.Close()
	if err := decodeLimitedJSON(response.Body, out); err != nil {
		return fmt.Errorf("decode Cloudflare API response: %w", err)
	}
	success, messages := inspectEnvelope(out)
	if response.StatusCode < 200 || response.StatusCode >= 300 || !success {
		message := strings.Join(messages, "; ")
		if message == "" {
			message = response.Status
		}
		return fmt.Errorf("cloudflare API rejected the request: %s", message)
	}
	return nil
}

func inspectEnvelope(value any) (bool, []string) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, nil
	}
	var envelope struct {
		Success bool       `json:"success"`
		Errors  []apiError `json:"errors"`
	}
	if json.Unmarshal(encoded, &envelope) != nil {
		return false, nil
	}
	messages := make([]string, 0, len(envelope.Errors))
	for _, item := range envelope.Errors {
		if item.Message != "" {
			messages = append(messages, item.Message)
		}
	}
	return envelope.Success, messages
}

func decodeLimitedJSON(reader io.Reader, value any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxCloudflareResponseBytes))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return nil
}
