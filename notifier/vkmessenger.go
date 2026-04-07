package notifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
)

const vkAPIBase = "https://api.vk.com/method"
const vkAPIVersion = "5.199"

// SendVKMessengerMessage sends alert through VK Messenger
func SendVKMessengerMessage(event models.Event, snapshot io.Reader, provider notifMeta) {
	profile := config.ConfigData.Alerts.VKMessenger[provider.index]
	status := &config.Internal.Status.Notifications.VKMessenger[provider.index]

	// Build notification message
	var message string
	if profile.Template != "" {
		message = renderMessage(profile.Template, event, "message", "VKMessenger")
	} else {
		message = renderMessage("plaintext", event, "message", "VKMessenger")
	}

	// Try to attach snapshot as photo if configured and available
	var attachment string
	if profile.SendSnap && event.HasSnapshot && snapshot != nil {
		var err error
		attachment, err = vkUploadPhoto(profile.Token, profile.PeerID, snapshot)
		if err != nil {
			log.Warn().
				Str("event_id", event.ID).
				Str("provider", "VKMessenger").
				Int("provider_id", provider.index).
				Err(err).
				Msg("Failed to upload snapshot to VK, sending text only")
		}
	}

	// Send message
	params := url.Values{}
	params.Set("peer_id", fmt.Sprintf("%d", profile.PeerID))
	params.Set("message", message)
	params.Set("random_id", fmt.Sprintf("%d", rand.Int63()))
	params.Set("access_token", profile.Token)
	params.Set("v", vkAPIVersion)
	if attachment != "" {
		params.Set("attachment", attachment)
	}

	resp, err := http.PostForm(vkAPIBase+"/messages.send", params)
	if err != nil {
		log.Warn().
			Str("event_id", event.ID).
			Str("provider", "VKMessenger").
			Int("provider_id", provider.index).
			Err(err).
			Msg("Unable to send alert")
		status.NotifFailure(err.Error())
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if err := vkCheckError(body); err != nil {
		log.Warn().
			Str("event_id", event.ID).
			Str("provider", "VKMessenger").
			Int("provider_id", provider.index).
			Err(err).
			Msg("Unable to send alert")
		status.NotifFailure(err.Error())
		return
	}

	log.Info().
		Str("event_id", event.ID).
		Str("provider", "VKMessenger").
		Int("provider_id", provider.index).
		Msg("Alert sent")
	status.NotifSuccess()
}

// vkUploadPhoto uploads a photo to VK and returns the attachment string
func vkUploadPhoto(token string, peerID int64, snapshot io.Reader) (string, error) {
	// Step 1: Get upload server URL
	params := url.Values{}
	params.Set("peer_id", fmt.Sprintf("%d", peerID))
	params.Set("access_token", token)
	params.Set("v", vkAPIVersion)

	resp, err := http.PostForm(vkAPIBase+"/photos.getMessagesUploadServer", params)
	if err != nil {
		return "", fmt.Errorf("getMessagesUploadServer: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if err := vkCheckError(body); err != nil {
		return "", fmt.Errorf("getMessagesUploadServer: %w", err)
	}

	var uploadServerResp struct {
		Response struct {
			UploadURL string `json:"upload_url"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &uploadServerResp); err != nil {
		return "", fmt.Errorf("parse upload server response: %w", err)
	}
	uploadURL := uploadServerResp.Response.UploadURL
	if uploadURL == "" {
		return "", fmt.Errorf("empty upload URL from VK")
	}

	// Step 2: Upload photo to the upload server
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("photo", "snapshot.jpg")
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err = io.Copy(part, snapshot); err != nil {
		return "", fmt.Errorf("copy snapshot: %w", err)
	}
	writer.Close()

	uploadResp, err := http.Post(uploadURL, writer.FormDataContentType(), &buf)
	if err != nil {
		return "", fmt.Errorf("upload photo: %w", err)
	}
	defer uploadResp.Body.Close()

	uploadBody, _ := io.ReadAll(uploadResp.Body)

	var uploadResult struct {
		Server int    `json:"server"`
		Photo  string `json:"photo"`
		Hash   string `json:"hash"`
	}
	if err := json.Unmarshal(uploadBody, &uploadResult); err != nil {
		return "", fmt.Errorf("parse upload response: %w", err)
	}
	if uploadResult.Photo == "" || uploadResult.Photo == "[]" {
		return "", fmt.Errorf("VK returned empty photo after upload")
	}

	// Step 3: Save photo and get attachment ID
	saveParams := url.Values{}
	saveParams.Set("server", fmt.Sprintf("%d", uploadResult.Server))
	saveParams.Set("photo", uploadResult.Photo)
	saveParams.Set("hash", uploadResult.Hash)
	saveParams.Set("access_token", token)
	saveParams.Set("v", vkAPIVersion)

	saveResp, err := http.PostForm(vkAPIBase+"/photos.saveMessagesPhoto", saveParams)
	if err != nil {
		return "", fmt.Errorf("saveMessagesPhoto: %w", err)
	}
	defer saveResp.Body.Close()

	saveBody, _ := io.ReadAll(saveResp.Body)
	if err := vkCheckError(saveBody); err != nil {
		return "", fmt.Errorf("saveMessagesPhoto: %w", err)
	}

	var saveResult struct {
		Response []struct {
			OwnerID int64 `json:"owner_id"`
			ID      int64 `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(saveBody, &saveResult); err != nil {
		return "", fmt.Errorf("parse save response: %w", err)
	}
	if len(saveResult.Response) == 0 {
		return "", fmt.Errorf("VK returned no saved photos")
	}

	photo := saveResult.Response[0]
	return fmt.Sprintf("photo%d_%d", photo.OwnerID, photo.ID), nil
}

// vkCheckError checks the VK API response body for API-level errors
func vkCheckError(body []byte) error {
	var errResp struct {
		Error struct {
			ErrorCode int    `json:"error_code"`
			ErrorMsg  string `json:"error_msg"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errResp); err != nil {
		return nil
	}
	if errResp.Error.ErrorCode != 0 {
		return fmt.Errorf("VK API error %d: %s", errResp.Error.ErrorCode, errResp.Error.ErrorMsg)
	}
	// Also check for response being a string error
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{\"error\"") {
		return fmt.Errorf("VK API error: %s", trimmed)
	}
	return nil
}
