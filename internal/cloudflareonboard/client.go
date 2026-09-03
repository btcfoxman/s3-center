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
	"strings"
)

const maxCloudflareResponseBytes = 2 << 20

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

type tokenVerification struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type bootstrapIdentity struct {
	ID        string
	Owner     string
	AccountID string
}

func (s *Service) discoverBootstrapAccount(
	ctx context.Context,
	client *http.Client,
	token, accountID string,
) ([]Account, bootstrapIdentity, error) {
	identity, err := s.verifyAccountToken(ctx, client, token, accountID)
	if err != nil {
		return nil, bootstrapIdentity{}, err
	}
	var accountEnvelope apiEnvelope[apiAccount]
	accountPath := "/accounts/" + url.PathEscape(accountID)
	if err := s.doAPI(ctx, client, http.MethodGet, accountPath, token, nil, &accountEnvelope); err != nil {
		return nil, bootstrapIdentity{}, err
	}
	if !strings.EqualFold(accountEnvelope.Result.ID, accountID) || strings.TrimSpace(accountEnvelope.Result.Name) == "" {
		return nil, bootstrapIdentity{}, errors.New("cloudflare returned invalid account details for the bootstrap token")
	}
	buckets, err := s.listBuckets(ctx, client, token, accountID)
	if err != nil {
		return nil, bootstrapIdentity{}, err
	}
	return []Account{{ID: accountID, Name: accountEnvelope.Result.Name, Buckets: buckets}}, identity, nil
}

func (s *Service) verifyAccountToken(
	ctx context.Context,
	client *http.Client,
	token, accountID string,
) (bootstrapIdentity, error) {
	var envelope apiEnvelope[tokenVerification]
	path := "/accounts/" + url.PathEscape(accountID) + "/tokens/verify"
	if err := s.doAPI(ctx, client, http.MethodGet, path, token, nil, &envelope); err != nil {
		return bootstrapIdentity{}, err
	}
	if envelope.Result.ID == "" || envelope.Result.Status != "active" {
		return bootstrapIdentity{}, errors.New("cloudflare account bootstrap token is not active")
	}
	return bootstrapIdentity{ID: envelope.Result.ID, Owner: "account", AccountID: accountID}, nil
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
	bootstrapToken string,
	account Account,
	bucket Bucket,
) (string, string, error) {
	permissionID, err := s.bucketWritePermissionID(ctx, client, bootstrapToken, account.ID)
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
	if err := s.doAPI(ctx, client, http.MethodPost, path, bootstrapToken, body, &envelope); err != nil {
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
	return "", errors.New("cloudflare did not expose the bucket-scoped R2 write permission; verify the bootstrap token permissions")
}

func (s *Service) deleteAccountToken(ctx context.Context, client *http.Client, bootstrapToken, accountID, tokenID string) error {
	var envelope apiEnvelope[struct {
		ID string `json:"id"`
	}]
	path := "/accounts/" + url.PathEscape(accountID) + "/tokens/" + url.PathEscape(tokenID)
	return s.doAPI(ctx, client, http.MethodDelete, path, bootstrapToken, nil, &envelope)
}

func (s *Service) deleteBootstrapToken(
	ctx context.Context,
	client *http.Client,
	bootstrapToken string,
	identity bootstrapIdentity,
) error {
	if identity.ID == "" {
		return errors.New("bootstrap token identifier is unavailable")
	}
	if identity.Owner != "account" || identity.AccountID == "" {
		return errors.New("bootstrap token ownership is unavailable")
	}
	return s.deleteAccountToken(ctx, client, bootstrapToken, identity.AccountID, identity.ID)
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
