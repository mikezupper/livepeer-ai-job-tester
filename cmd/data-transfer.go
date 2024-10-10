package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"livepeer-job-tester/internal/types"
	"net/http"
	"time"
)

func main() {
	sourceLeaderboardURL := flag.String("source-api", "http://localhost:8080", "Source Leaderboard API Server URL")
	leaderboardURL := flag.String("api", "http://localhost:8080", "Destination Leaderboard API Server URL")
	gatewayURL := flag.String("gw", "http://localhost:7935", "Livepeer Gateway Cli Endpoint URL")
	apiSecretKey := flag.String("secret", "your-api-secret-key", "Destination Leaderboard API Secret Key")
	flag.Parse()

	fmt.Printf("Starting Data Transfer from [%s].  Gateway [%s] and Leaderboard API  [%s] secret key [*****]\n", *sourceLeaderboardURL, *gatewayURL, *leaderboardURL)

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Timeout:   120 * time.Second,
		Transport: tr,
	}

	// Fetch all Orchs
	orchestrators, err := FetchOrchestrators(client, *gatewayURL)
	if err != nil {
		fmt.Println("failed to fetch orchs = %v", err)

		return
	}

	// For each orch, call the Livepeer Prod raw_stats JSON endpoint
	for _, orch := range orchestrators {
		url := fmt.Sprintf("%s/api/raw_stats?orchestrator=%s", *sourceLeaderboardURL, orch.Address)
		resp, err := http.Get(url)
		if err != nil {
			fmt.Printf("Error making request for %s: %v\n", orch.Address, err)
			continue
		}
		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("Error reading response body for %s: %v\n", orch.Address, err)
			continue
		}

		var result map[string][]Stats
		if err := json.Unmarshal(body, &result); err != nil {
			fmt.Printf("Error unmarshaling JSON for %s: %v\n", orch.Address, err)
			continue
		}

		fmt.Printf("Data for Orchestrator %s:\n", orch.Address)
		for region, stats := range result {
			fmt.Printf("Region: %s\n", region)
			for _, stat := range stats {
				fmt.Printf("Segments Sent: %d, Success Rate: %.2f\n", stat.SegmentsSent, stat.SuccessRate)
				err := PostStats(client, *leaderboardURL, *apiSecretKey, stat)
				if err != nil {
					fmt.Printf("error posting stats: %v \n", err)
					continue
				}
			}

		}

	}
}

func FetchOrchestrators(client *http.Client, gatewayUrl string) ([]types.Orchestrator, error) {
	// Implement the logic to fetch orchestrators
	url := fmt.Sprintf("%s/registeredOrchestrators", gatewayUrl)
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[FetchOrchestrators] response contained a non-200 status code: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var orchestrators []types.Orchestrator
	err = json.Unmarshal(body, &orchestrators)
	if err != nil {
		return nil, err
	}

	return orchestrators, nil
}

type Error struct {
	ErrorCode string `json:"error_code"`
	Count     int    `json:"count"`
}

type Stats struct {
	Region           string  `json:"region"`
	Orchestrator     string  `json:"orchestrator"`
	SegmentsSent     int     `json:"segments_sent"`
	SegmentsReceived int     `json:"segments_received"`
	SuccessRate      float64 `json:"success_rate"`
	SegDuration      float64 `json:"seg_duration"`
	UploadTime       float64 `json:"upload_time"`
	DownloadTime     float64 `json:"download_time"`
	TranscodeTime    float64 `json:"transcode_time"`
	RoundTripTime    float64 `json:"round_trip_time"`
	Errors           []Error `json:"errors"`
	Timestamp        int64   `json:"timestamp"`
}

func PostStats(client *http.Client, url string, secret string, stats Stats) error {
	input, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	statsUrl := fmt.Sprintf("%s/api/post_stats", url)
	req, err := http.NewRequest("POST", statsUrl, bytes.NewBuffer(input))
	if err != nil {
		return err
	}

	hash := hmac.New(sha256.New, []byte(secret))
	hash.Write(input)
	req.Header.Set("Authorization", hex.EncodeToString(hash.Sum(nil)))
	req.Header.Set("Content-Type", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return err
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New(fmt.Sprintf("invalid response status code from POST STATS [%v]", res.StatusCode))
	}

	fmt.Printf("Posted Transcoding stats for orchestrator %s - success=%v   latency=%v   transcodeTime=%v \n", stats.Orchestrator, stats.SuccessRate, stats.RoundTripTime, stats.TranscodeTime)
	return nil
}
