# The Livepeer AI Job Tester

## Overview
The **Livepeer AI Job Tester** is a versatile tool for executing AI test jobs across all Livepeer Orchestrators on the Livepeer AI Network. Its purpose is to ensure each Orchestrator is tested only on the pipelines and models they support, allowing seamless testing and the production of network reliability metrics.

For a visual walkthrough of the live video path, see [docs/live_video_flow.md](/home/julian/Documents/development/spe-work/livepeer-ai-job-tester/docs/live_video_flow.md).

## Key Features

- **Targeted Testing**  
  Each Orchestrator is only tested for the pipelines and models it supports, ensuring efficient and accurate testing.

- **Configuration-Based Flexibility**  
  Easily manage support for multiple pipelines and models through a configuration-based system. New pipelines and models can be added over time without modifying the core code.

- **Integration with Livepeer Leaderboard**  
  Integrates with the [Livepeer Leaderboard Serverless API](https://github.com/livepeer/leaderboard-serverless)) for enhanced reporting and Orchestrator performance tracking.

- **Decoupled Architecture**  
  Operates independently of the `go-livepeer` Gateway Node, using HTTP REST endpoints to send AI jobs directly to a separately deployed Gateway node.

- **Docker Support**  
  Fully Dockerized for easy setup and deployment across various environments.

- **Scheduled Job Execution**  
  Supports `Crontab` scheduling to automate the execution of test jobs at regular intervals.

### Figure 1 - AI Job Testing Architecture
This repository covers the *"Livepeer AI Job Tester"* box in the AI Job Testing architecture.

![Overall Job Testing Architecture](docs/logical_architecture.png)

### Figure 2 - Application Architecture
The key components of the AI Job Tester application

![Job Tester Application Architecture](docs/application_architecture.png)

#### AI Job Tester App
The main entrypoint for the application. This component is responsible for controlling the flow of the entire application
(find/test orchestrators, fetch pipeline/models, tracking job metrics, record job stats).

* **Embedded Webhook Server** - The HTTP endpoint that allows the Livepeer Gateway to determine which Orchestrator should be selected. The Livepeer Gateway refers to this as the "Orch Webhook URL".
* **Livepeer Client Service** - This component handles all HTTP Client interactions:
  1. _Livepeer Gateway_ -  Registered Orchestrators, Network Capabilities - Pipelines/Models, and AI Job processing
  2. _Leaderboard API Server_ - Post AI Job stats to the Leaderboard API Server (see Figure 1)

#### Livepeer Gateway
As the AI Job Tester iterates through the list of Orchestrators, it notifies the Livepeer Gateway about which Orchestrator to run the test scenarios against.
To enable this behavior, the Gateway uses `orchWebhookUrl`, `aiSessionTimeout`, `aiTesterGateway` and `webhookRefreshInterval`.

**_Important:_** The following pull request must be merged to run the AI Job Tester gateway

[#3236 - Enable Single Orchestrator AI Job Testing Support for Gateway Nodes](https://github.com/livepeer/go-livepeer/pull/3236)

#### Leaderboard API Server
For more details see the [docs](https://github.com/mikezupper/livepeer-leaderboard-serverless/tree/tasks/livepeer.cloud/proposal2/add-ai-job-support)

## Build the Application

### Prerequisites

- Go Lang
- A Running Livepeer Gateway (configured for AI Test Jobs)

### Clone the Repo

Clone the Git Repo

`git clone https://github.com/mikezupper/livepeer-ai-job-tester.git`

Change to the Repo Directory

`cd livepeer-ai-job-tester`

### Build the Code

The following are environment variables that are needed to build:

`export CGO_ENABLED=0`

`export GOOS=linux`

`export GOARCH=amd64`

Build the binary

`go build -o ai-job-tester ./cmd/ai-job-tester.go`

### Configuration the Application
The application has several one command line argument `-f <full path to config file>`

The example file is located `configs/config.json`

#### config.json

This file configures the AI Job Tester application.

| Config Entry               | Description                                                                                                                                                                                        |
|----------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `region`                   | The region code. _(default: NYC)_ (see [Region API Reference](https://github.com/mikezupper/livepeer-leaderboard-serverless/tree/tasks/livepeer.cloud/proposal2/add-ai-job-support#api-reference)) |
| `jobType`                  | The job type _(default: ai)_. Currently supports `ai`. New Types maybe be added in the future.                                                                                                     |
| `internalWebServerPort`    | The EmbeddedWebServer (Orch Webhook URL) will listen on this port _(default: 7934)_.                                                                                                               |
| `internalWebServerAddress` | The EmbeddedWebServer (Orch Webhook URL) will listen on this network ip address _(default: 0.0.0.0)_.                                                                                              |
| `metricsApiEndpoint`       | The URL to the Leaderboard API [post_stats endpoint](https://github.com/mikezupper/livepeer-leaderboard-serverless/tree/tasks/livepeer.cloud/proposal2/add-ai-job-support#api-reference)           |
| `metricsSecret`            | The `SECRET` key used by the Leaderboard API Server.                                                                                                                                               |
| `broadcasterJobEndpoint`   | The URL to the Livepeer Gateway AI Job Endpoint.                                                                                                                                                   |
| `broadcasterCliEndpoint`   | The URL to the Livepeer Gateway CLI Endpoint. See note regarding `Orchestrator Discovery for Live Jobs`.                                                                                           |
| `broadcasterRequestToken`  | Optional: A Unique Token to send with each AI Job.                                                                                                                                                 |
| `pipelines`                | The configuration of each model and pipeline. This includes the API input parameters used for AI Job submission.                                                                                   |
| `liveVideo.mediaServerURL`      | Base ingest URL used when pushing the test stream (the tester appends a dynamic stream key) and by the playback probe when reading the transformed output (the tester appends the dynamic stream key followed by `-out`). |
| `liveVideo.testVideoPath`  | The path to the video asset to be used for live video tests.                                                                                                                                       |
| `liveVideo.testDurationSeconds` | Duration, in seconds, to collect live metrics for each test. _(default: 30)_                                                                                                                       |
| `liveVideo.statusPollTimeoutSeconds` | Maximum seconds to wait for the Gateway `/live/video-to-video/{stream}/status` endpoint to report the stream as ready before failing the test. _(default: 20)_                                     |
| `liveVideo.statusPollIntervalSeconds` | Interval, in seconds, between status poll requests to check if the stream is ready. _(default: 1)_                                                                                                 |
| `liveVideo.metricRetryDelayMilliseconds` | Delay, in milliseconds, between retry attempts when collecting metrics from the stream. _(default: 200)_                                                                                           |
| `liveVideo.maxMetricAttempts` | Maximum number of attempts to collect metrics from the stream before failing. _(default: 300)_                                                                                                     |
| `liveVideo.maxProbeAttempts` | Maximum attempts made to probe the stream playback when scoring the stream. _(default: 5)_                                                                                                         |
| `liveVideo.orchMapping`    | Map of orchestrator addresses to one or more live URIs. Each override is tested when a live pipeline runs, replacing the on-chain ServiceURI only for live jobs.                                   |
| `liveVideo.targetFPS`      | Expected steady-state FPS for a healthy stream.                                                                                                                                                    |
| `liveVideo.maxInitialLatencySeconds` | Maximum acceptable time-to-first-frame used when scoring the stream.                                                                                                                               |

_**Note:**_ pipelines that require input assets (images or audio) the test files are located in the `tests-assets/` folder. When adding new pipelines, make sure to update the ai job submission logic in `internal/server/server.go` `SendTestJob` function.

### Live Video Pipeline Configuration

`live-video-to-video` is the only pipeline that uses the multi-prompt layout. Its config must:

- set `"live": true`
- optionally provide shared base params in `parameters`
- provide one or more `promptVariants`
- assign every prompt variant an `id`, `complexity` (`low|medium|high`), and `parameters`

Example:

```json
{
  "name": "Live video to video",
  "uri": "live-video-to-video",
  "live": true,
  "promptVariants": [
    {
      "id": "watercolor-low",
      "complexity": "low",
      "parameters": {
        "prompt": "watercolor painting style"
      }
    },
    {
      "id": "cyberpunk-medium",
      "complexity": "medium",
      "parameters": {
        "prompt": "a cinematic cyberpunk street with neon reflections and rainy atmosphere"
      }
    }
  ]
}
```

Non-live pipeline config does not change.

### Orchestrator Discovery for Live Jobs

The Gateway cannot rely on the on-chain Service Registry to discover live AI capabilities and all Orchestrator URIs. As such, there is a separate Docker Compose project (`docker-compose-live-video.yml`) that runs two Gateway, one for running test jobs and another for retrieving the capabilities of a pre-configured list of live video enable Orchestrators. The second Gateway (registry) ensures the first Gateway (tester) can be dynamic set to a specific Orchestrator URI without interfering with service discovery. This is a short term fix until the service registry is improved.

To leverage this, you must configure this second Gateway with the exact Orchestrator service URIs that should receive live video tests in the `orchAddr` flag. This is used in combination with `liveVideo.orchMapping` to find the orchestrator and override the published ServiceURI with the full list of URIs that should be exercised for live video. Lastly, this second gateway should be configured as the `broadcasterCliEndpoint` in your config.json as well.

### Mediamtx Integration

`configs/mediamtx/mediamtx.yml` defines two relevant RTMP paths with dynamic stream key support:

- `~^aiJobTesterStream-[^-]+-.+-[0-9a-f]{8}-[0-9]+$` – Matches ai-job-tester ingest stream IDs and uses `runOnReady` to invoke the Gateway CLI as soon as the tester pushes the input RTMP stream. Update this line to point to the URI of the tester Gateway.
- `~^aiJobTesterStream-[^-]+-.+-[0-9a-f]{8}-[0-9]+-out$` – Records only the plain playback output stream under `recordings/live-video/output/<stream_id>-out/`, allowing you to inspect the final video produced by the orchestrator without also recording request-scoped internal output paths.

Ensure the Mediamtx container shares the same network namespace as the Gateway so these hooks can execute successfully. The checked-in [`docker-compose-media-mtx.yml`](/home/julian/Documents/development/spe-work/livepeer-ai-job-tester/docker-compose-media-mtx.yml) already bind-mounts [`./recordings`](/home/julian/Documents/development/spe-work/livepeer-ai-job-tester/recordings) to `/app/recordings` inside the container so recordings are visible on the host.

Also, the `aiJobTesterStream` stream key prefix is defined in the go code and any changes to it must be updated in this config as well!

### Live RTMP URL Structure

The tester publishes the live input stream to Mediamtx as:

```text
rtmp://<mediamtx-host>:1935/<streamId>?pipeline=<model-id>&streamId=<streamId>&orchestrator=<service-uri>&params=<urlencoded-json>
```

Where:

- `<streamId>` must match the Mediamtx regex path, which in this repo means it must begin with `aiJobTesterStream-`
- `pipeline` is the capability-advertised live selector from the orch payload, not the pipeline URI `live-video-to-video`
- `orchestrator` is the full service URI for the target orch, for example `https://orch.example:8935`
- `params` is a single JSON object encoded into one query parameter, for example `{"prompt":"watercolor painting style"}`
- an optional nested `params.model_id` can be used as a runner-specific base-model override; it is distinct from the top-level `pipeline` selector

Concrete example:

```text
rtmp://live-video-to-video-mediamtx:1935/aiJobTesterStream-low-watercolor-1a2b3c4d-1711570000000000000?pipeline=streamdiffusion-model&streamId=aiJobTesterStream-low-watercolor-1a2b3c4d-1711570000000000000&orchestrator=https%3A%2F%2Forch.example%3A8935&params=%7B%22prompt%22%3A%22watercolor%20painting%20style%22%7D
```

For the checked-in local manual-test config in `configs/config-live-video-to-video-local.json`, the URL fields map like this:

- `liveVideo.mediaServerURL` supplies `rtmp://live-video-to-video-mediamtx:1935`
- `liveVideo.orchMapping` supplies the `orchestrator=` service URI
- `pipelines[].promptVariants[].parameters` plus `pipelines[].parameters` are merged into the JSON carried by `params=`
- the `pipeline=` query value comes from live capability discovery and is the orch-advertised selector, not a static config field

Observed local run example:

```text
rtmp://live-video-to-video-mediamtx:1935/aiJobTesterStream-low-manual-watercolor-low-f502d679-1774559694562853756?pipeline=streamdiffusion-sdxl-v2v&streamId=aiJobTesterStream-low-manual-watercolor-low-f502d679-1774559694562853756&orchestrator=https%3A%2F%2Fstream2.speedybird.xyz%3A18938&params=%7B%22prompt%22%3A%22watercolor%20painting%20style%22%7D
```

In that example, the trailing numeric suffix in `streamId` is generated per run, while the `pipeline=` value came from the currently discovered orch capability response.

The transformed output is then read from the corresponding `-out` path:

```text
rtmp://<mediamtx-host>:1935/<streamId>-out
```

Implementation notes from this repo:

- If `liveVideo.mediaServerURL` ends with `/`, the tester appends `<streamId>`
- If `liveVideo.mediaServerURL` contains `{streamKey}`, the tester replaces that token with `<streamId>`
- Otherwise the tester publishes to `<mediaServerURL>/<streamId>`
- Mediamtx forwards the original query string unchanged as `$MTX_QUERY`, and the Gateway start hook receives it as `query=$MTX_QUERY`

### Live Design Principles

- Live prompt scenarios are config-driven and expanded per orch/model/prompt variant.
- The live payload is encoded into the RTMP query string once, then forwarded unchanged by Mediamtx through `$MTX_QUERY`.
- Gateway routing fields stay top-level in the RTMP query (`pipeline`, `streamId`, `orchestrator`), while inference params are encoded into a single `params=<json>` query field.
- Prompt verification means the worker reported the expected `last_params_hash`; it does not claim semantic prompt adherence.
- Busy/capped orchs are deferred and eventually marked `unscored`, not failed.
- MediaMTX recordings are the source of truth for saved live-output artifacts.

### Live AI Video Data Flow

1. The tester expands each `live-video-to-video` config into one prompt scenario per `promptVariants[]` entry.
2. For each live scenario, the tester merges `pipeline.parameters` with the prompt variant params and computes a canonical params hash.
3. The tester builds an RTMP ingest URL with:
   - `pipeline=<capability-advertised live selector>`
   - `streamId=<prompt-aware stream id>`
   - `orchestrator=<target service URI>`
   - `params=<json>`
4. Mediamtx receives that RTMP publish and forwards the original query string to the gateway via `runOnReady` and `query=$MTX_QUERY`.
5. The gateway parses the forwarded query string, extracts the single `params` field, and starts the live worker.
6. The tester polls `/live/video-to-video/{stream}/status`, parses the JSON body, and uses that body both for busy/capacity classification and later prompt verification.
7. Once the stream is ready enough to probe, the tester runs a single `ffmpeg` playback probe against the output URL to collect FPS/latency metrics.
8. If a prompt fails before the stream ever becomes ready and the terminal status still indicates a capacity-style startup rejection, the tester gives the failed attempt a short teardown window, rebuilds the live run with a fresh `stream_id`, and retries that same prompt with a small bounded backoff instead of immediately treating it as a final failure.
9. After each successful prompt in a multi-prompt bundle, the tester first applies a short fixed settle delay, then gives that same orch/model a short bounded readiness window before sending the next prompt. If capacity does not return quickly, the remaining prompts are deferred instead of being sent immediately into a saturated orch.
10. After the run, the tester fetches terminal live status snapshots and confirms prompt acceptance by matching the expected params hash to `inference_status.last_params_hash`.
11. Busy/capped orchs are deferred to a later pass; exhausted retries or bundles that are still cooling down at the end of the current run become `unscored` rather than failed.

### Live Video Metrics & Scoring

Live runs produce a set of metrics that are derived directly from the sampled frame data:

- **Frame arrival window** – The playback probe records per-frame presentation timestamps (PTS) and the wall-clock arrival time. The tester uses the first/last PTS as the primary duration source and falls back to arrival timing or the configured test duration if necessary.
- **Average FPS** – Calculated from the number of frames divided by the PTS-derived duration so the result is independent of buffering behaviour in the playback probe.
- **Average latency** – Per-frame latency is measured as `arrivalSinceIngest - pts`; the average represents the end-to-end delay once the stream is flowing.
- **Gateway readiness** – Time spent polling `/live/video-to-video/{stream}/status` until the Gateway returns a usable status snapshot without a terminal gateway error. This duration is emitted as `gateway_ready_seconds` and is subtracted from the latency score so legitimate warm-up time is not penalized.
- **Initial latency** – The time from ingest start until the first frame arrives. The latency score uses the latency beyond the measured Gateway warm-up window.

The normalized score that surfaces in the stats payload combines the above measurements:

- **FPS component** – Compares the measured FPS to `liveVideo.targetFPS` and clamps the ratio to `0..1`.
- **Latency component** – Compares the latency observed after the Gateway reported readiness to `liveVideo.maxInitialLatencySeconds`, also clamped to `0..1`. If the max is omitted, latency defaults to a perfect score.
- **Final score** – A weighted average (`0.6 * FPS + 0.4 * latency`) posted in the live stats payload when the run is scored.

Choose `statusPollTimeoutSeconds` to reflect how long the Gateway normally needs to prepare a pipeline. The tester fails the run if the Gateway never reports readiness within that window; otherwise the measured warm-up time is subtracted from the latency score so only post-ready latency impacts the final score. `maxInitialLatencySeconds` should capture how quickly the first frame should arrive once the stream is ready.

### Busy Orchestrators and Unscored Runs

For live video tests the tester uses only live-path signals:

- refreshed `getNetworkCapabilities` data before each pass
- parsed `/live/video-to-video/{stream}/status` responses during and after the run

Behavior:

- `capacity == 0` and `capacity_in_use > 0` is treated as busy and deferred
- `capacity == 0` and `capacity_in_use == 0` is treated as indeterminate capacity and deferred
- `OrchestratorCapped`, `OrchestratorBusy`, or `insufficient capacity` in live status are treated as busy
- `no orchestrators available` during normal runtime is treated cautiously and only mapped to a defer outcome when corroborated by fresh live capacity data
- if a prompt dies before readiness with one of those startup-capacity signals, the tester waits briefly for the failed attempt to disappear, rebuilds the run with a fresh `stream_id`, and retries that same prompt with a short bounded backoff
- if those startup retries are exhausted and the gateway still reports a capacity-style admission failure, the prompt is deferred/unscored instead of being posted as a scored failure
- after a successful prompt, the tester first waits through a short settle delay so gateway/orch teardown can catch up, then performs a short bounded readiness check before sending the next prompt to the same orch/model
- if that readiness window expires, the remaining prompts for that orch/model bundle are deferred and the bundle enters a short cooldown for the rest of the current run
- if the current run would otherwise just spin on cooling-down bundles, those prompts are finalized as `unscored` instead of being retried immediately again

Deferred bundles are retried for a bounded number of passes. If an orch never frees up, the run is posted as `unscored` so downstream systems can see that the attempt happened without treating a loaded orch like a failed one.

### Prompt Verification

Prompt verification is intentionally narrow:

- `confirmed` means the tester’s canonical params hash matched `inference_status.last_params_hash` from the live status API
- `unverified` means the stream ended before the tester observed that confirmation

This confirms that the worker accepted the intended prompt payload. It does **not** prove that the pixels semantically match the prompt.

### Local Runs and Manual Debugging

`docker-compose-live-video.yml` is still the production/cron-oriented setup. For local one-shot runs, use the companion override file so you can keep production unchanged while replacing the cron entrypoint with a direct invocation plus a post-run sleep for manual inspection:

```bash
docker compose \
  -f docker-compose-live-video.yml \
  -f docker-compose-live-video.local.yml \
  up --build
```

Useful local override knobs:

- `CONFIG_FILE` defaults to `/app/local-configs/config-live-video-to-video-pipelines.json`
- `LIVE_MANUAL_ATTACH_SECONDS` defaults to `20`
- `LOCAL_STARTUP_RETRIES` defaults to `4`
- `LOCAL_STARTUP_RETRY_DELAY_SECONDS` defaults to `10`
- `POST_RUN_SLEEP_SECONDS` defaults to `600`
- live prompt startup retry attempts inside the tester are currently a code-level default in [`internal/server/live_retry.go`](/home/julian/Documents/development/spe-work/livepeer-ai-job-tester/internal/server/live_retry.go), not a JSON config field. The current default is `2` retries per prompt after the initial attempt.

A checked-in manual-testing config is available at [`configs/config-live-video-to-video-local.json`](/home/julian/Documents/development/spe-work/livepeer-ai-job-tester/configs/config-live-video-to-video-local.json). It narrows the run to one orch/service URI and lengthens the stream duration for manual playback verification.
It also sets `disableStatsPosting: true`, so local runs log the final stats payload instead of trying to POST to the leaderboard API.

Example targeting the checked-in local manual config and keeping the container alive for 15 minutes after the run:

```bash
CONFIG_FILE=/app/local-configs/config-live-video-to-video-local.json \
LIVE_MANUAL_ATTACH_SECONDS=30 \
LOCAL_STARTUP_RETRIES=6 \
LOCAL_STARTUP_RETRY_DELAY_SECONDS=10 \
POST_RUN_SLEEP_SECONDS=900 \
docker compose \
  -f docker-compose-live-video.yml \
  -f docker-compose-live-video.local.yml \
  up --build
```

The override builds the local image from this repo and bind-mounts `./configs` into the container at `/app/local-configs`.
It also retries the one-shot tester command locally if the Gateway is still booting, which helps with transient `connection refused` races during `docker compose up`.

If you do not want to use Docker for a one-off local run, you can still execute the binary directly instead of using the cron-based container entrypoint:

```bash
go build -o ai-job-tester ./cmd/ai-job-tester.go
./ai-job-tester -f configs/config-live-video-to-video-local.json -liveManualAttachSeconds 30
```

Expected local topology:

- Mediamtx configured with the provided `configs/mediamtx/mediamtx.yml`
- a tester gateway serving `broadcasterJobEndpoint`
- a discovery/CLI gateway serving `broadcasterCliEndpoint`
- the tester binary pointed at the live-video config file

For manual inspection:

1. Start from [`configs/config-live-video-to-video-local.json`](/home/julian/Documents/development/spe-work/livepeer-ai-job-tester/configs/config-live-video-to-video-local.json).
2. Update `liveVideo.orchMapping` so it points at the orch and service URI you want to inspect.
3. Adjust `liveVideo.testDurationSeconds` if you want a shorter or longer manual watch window.
4. Ensure MediaMTX is started with [`docker-compose-media-mtx.yml`](/home/julian/Documents/development/spe-work/livepeer-ai-job-tester/docker-compose-media-mtx.yml). That compose file bind-mounts [`./recordings`](/home/julian/Documents/development/spe-work/livepeer-ai-job-tester/recordings) to `/app/recordings` inside the MediaMTX container, and [`configs/mediamtx/mediamtx.yml`](/home/julian/Documents/development/spe-work/livepeer-ai-job-tester/configs/mediamtx/mediamtx.yml) records plain playback `-out` streams under `recordings/live-video/output/<stream_id>-out/`.
5. Run the tester once with the Compose override or direct binary.
6. Use the logged `stream_id` or `playback_url` to find the recording. The output stream will be written under a path like `recordings/live-video/output/<stream_id>-out/<timestamp>.ts`. When `-liveManualAttachSeconds` is non-zero, the tester also logs a manual attach window message and waits before metric collection begins.
   If no recording directory appears for a prompt, the usual cause is that the live output stream never came online for that prompt, not that MediaMTX failed after recording had already started.
7. Open the playback URL with an external player such as:
   - `ffplay <playback_url>`
   - VLC using the same RTMP/HLS/WebRTC path
8. When you are done inspecting, stop the local stack with:

```bash
docker compose \
  -f docker-compose-live-video.yml \
  -f docker-compose-live-video.local.yml \
  down
```

Useful live-debug fields in the posted/logged payload:

- `test_outcome`
- `prompt_verification`
- `prompt_confirmed`
- `stream_valid`
- `stream_id`
- `params_hash`
- `defer_attempts`

##### Example Configuration
```json
{
  "region": "NYC",
  "jobType" : "ai",
  "internalWebServerPort": "7934",
  "internalWebServerAddress": "0.0.0.0",
  "metricsApiEndpoint": "https://localhost:8080/api/post_stats",
  "metricsSecret": "my-secret-key",
  "broadcasterJobEndpoint": "http://localhost:8935",
  "broadcasterCliEndpoint": "http://localhost:7935",
  "broadcasterRequestToken": "None",
  "pipelines": [
    {
      "name": "Segment anything 2",
      "uri": "segment-anything-2",
      "capture_response": false,
      "contentType": "multipart/form-data",
      "parameters": {
        "box": "[380.50, 130.00, 651.50, 479.00]",
        "multimask_output": true,
        "return_logits": true,
        "normalize_coords": true,
        "safety_check": false
      }
    },
    {
      "name": "Text to image",
      "uri": "text-to-image",
      "capture_response": true,
      "contentType": "application/json",
      "parameters": {
        "prompt": "a bear",
        "width": 512,
        "height": 512,
        "num_images_per_prompt": 1,
        "num_inference_steps": 20,
        "guidance_scale": 2,
        "safety_check": false
      }
    },
    {
      "name": "Image to image",
      "uri": "image-to-image",
      "capture_response": true,
      "contentType": "multipart/form-data",
      "parameters": {
        "guidance_scale": 2,
        "image_guidance_scale": 2,
        "num_images_per_prompt": 1,
        "num_inference_steps": 20,
        "prompt": "a bear",
        "safety_check": false,
        "strength": 1
      }
    },
    {
      "name": "Image to video",
      "uri": "image-to-video",
      "capture_response": true,
      "contentType": "multipart/form-data",
      "parameters": {
        "width": 1024,
        "height": 576,
        "fps": 8,
        "motion_bucket_id": 127,
        "noise_aug_strength": 0.065
      }
    },
    {
      "name": "Upscale",
      "uri": "upscale",
      "capture_response": true,
      "contentType": "multipart/form-data",
      "parameters": {
        "prompt": "a bear",
        "width": 512,
        "height": 512,
        "num_images_per_prompt": 1,
        "num_inference_steps": 20,
        "guidance_scale": 2,
        "safety_check": false
      }
    },
    {
      "name": "Audio to text",
      "uri": "audio-to-text",
      "capture_response": true,
      "contentType": "multipart/form-data",
      "parameters": {
      }
    },
    {
      "name": "Llm",
      "uri": "llm",
      "capture_response": true,
      "contentType": "multipart/form-data",
      "parameters": {
        "max_tokens": 256,
        "prompt": "how many characters are in an ethereum address?"
      }
    },
    {
      "name": "Live video to video",
      "uri": "live-video-to-video",
      "capture_response": false,
      "contentType": "application/json",
      "live": true,
      "promptVariants": [
        {
          "id": "watercolor-low",
          "complexity": "low",
          "parameters": {
            "prompt": "watercolor painting style"
          }
        },
        {
          "id": "cyberpunk-medium",
          "complexity": "medium",
          "parameters": {
            "prompt": "a cinematic cyberpunk street with neon reflections and rainy atmosphere"
          }
        }
      ]
    }
  ]
}
```

## Docker
The use of docker is encouraged but not required.

### Build the Image

`docker build ai-job-tester:latest .`

## Run the Application

To run the AI Job Tester application, you will need `docker compose`.

The `docker-compose.yml` will allow you to run the applications needed: AI Job Tester and Livepeer Gateway.
You must create the following docker volumes (and configure them appropriately)

_ai-job-tester_ - stores the `configs/config.json` file needed to run `ai-job-tester`. You must configure the file and place in the volume's directory.

`docker volume create ai-job-tester`

_tester-gateway-lpData_ - The Livepeer Gateway's `.lpData` folder. You must configure the required livepeer files and place in the volume's directory.

`docker volume create tester-gateway-lpData`

### Job Scheduling

The `ai-job-tester` docker image allows job scheduling using Linux `crontab`. `The docker-compose.yml` file has an environment variable to allow custom schedules.


Example runs every hour on the 0 minute: 

`- CRONTAB_SCHEDULE=0 */1 * * *`

### docker-compose.yml
```
services:
  ai-job-tester:
    image: ai-job-tester:latest
    container_name: "ai-job-tester"
    volumes:
      - ai-job-tester:/app/configs
    environment:
      - TZ=UTC
      - CRONTAB_SCHEDULE=0 */1 * * *
      - CONFIG_FILE=/app/configs/config.json
    depends_on:
      - tester-gateway

  tester-gateway:
    image: tztcloud/go-livepeer:v0.7.9-ai.3-v0.0.11
    restart: unless-stopped
    hostname: tester-gateway
    container_name: tester-gateway
    volumes:
      - tester-gateway-lpData:/root/.lpData
    environment:
      - LIVEPEER_OS_HTTP_TIMEOUT=8s
    command: '-ethUrl=YOUR_RPC_URL
              -ethPassword=/root/.lpData/eth-secret.txt
              -ethKeystorePath=/root/.lpData
              -network=arbitrum-one-mainnet
              -serviceAddr=ai-tester-gateway:8935
              -cliAddr=ai-tester-gateway:7935
              -gateway=true
              -monitor=true
              -maxPricePerUnit=125000000
              -maxTotalEV=100000000000000
              -v=5
              -pixelsPerUnit=1
              -blockPollingInterval=20
              -httpIngest=true
              -httpAddr=0.0.0.0:8935
              -orchMinLivepeerVersion=v0.7.9-ai.3
              -aiTesterGateway=true
              -discoveryTimeout=100ms
              -webhookRefreshInterval=0
              -aiSessionTimeout=0
              -orchWebhookUrl=http://ai-job-tester:7934/orchestrators
              '

volumes:
  tester-gateway-lpData:
    external: true

  ai-job-tester:
    external: true
```

**_Note:_** Take note of the configuration flags that are needed to run a Livepeer Gateway in "Job Test Mode"
Environment Variables:
`LIVEPEER_OS_HTTP_TIMEOUT=8s`

Livepeer Startup Flags
`-aiTesterGateway=true`
`-discoveryTimeout=1000ms`
`-webhookRefreshInterval=0`
`-aiSessionTimeout=0`
`-orchWebhookUrl=http://ai-job-tester:7934/orchestrators`
