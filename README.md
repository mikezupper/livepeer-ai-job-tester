# The Livepeer AI Job Tester

## Overview
The **Livepeer AI Job Tester** is a versatile tool for executing AI test jobs across all Livepeer Orchestrators on the Livepeer AI Network. Its purpose is to ensure each Orchestrator is tested only on the pipelines and models they support, allowing seamless testing and the production of network reliability metrics.

For a visual walkthrough of the live video path, see [docs/live_video_flow.md](docs/live_video_flow.md).

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

* **Job Runner** - Fetches network capabilities from the Gateway CLI endpoint, builds an execution plan per orchestrator/pipeline/model, and dispatches test jobs. This component has no inbound HTTP listener; it makes only outbound HTTP requests.
* **Livepeer Client Service** - This component handles all HTTP Client interactions:
  1. _Livepeer Gateway_ -  Registered Orchestrators, Network Capabilities - Pipelines/Models, and AI Job processing
  2. _Leaderboard API Server_ - Post AI Job stats to the Leaderboard API Server (see Figure 1)

#### Livepeer Gateway
As the AI Job Tester iterates through the list of Orchestrators, it notifies the Livepeer Gateway about which Orchestrator to run the test scenarios against.
To enable this behavior, the Gateway uses the `LIVEPEER_TESTER_GATEWAY_ENABLED` flag.

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

### Configuring the Application

#### Command-Line Flags

| Flag | Description |
|------|-------------|
| `-f <path>` | Path to the JSON config file. _(default: `configs/config.json`)_ |
| `-liveManualAttachSeconds <n>` | Local-only: delay in seconds after live stream readiness before metrics collection begins. Gives you time to open the playback URL in a player for manual inspection. _(default: 0)_ |

#### Environment Variables

| Variable | Description |
|----------|-------------|
| `TEST_INDIVIDUAL_ORCHESTRATORS` | When set to any non-empty value, the tester omits the `orchestrator=` parameter from both the job request body and the broadcaster job endpoint URL. This allows the Gateway to select an orchestrator via normal network routing rather than pinning to a specific one. Used by the `live-network` Docker Compose profile. |
| `CRONTAB_SCHEDULE` | Cron expression controlling how often the tester runs inside the container. Set in `docker-compose.yml` per profile. |
| `RUN_IMMEDIATE` | When `true`, runs the tester once immediately on container start instead of waiting for the first cron tick. The container exits cleanly after the run. |
| `CONFIG_FILE` | Path inside the container to the JSON config file. Set in `docker-compose.yml` per profile. |

The example config file is located at `configs/config.json`

#### config.json

This file configures the AI Job Tester application.

| Config Entry               | Description                                                                                                                                                                                        |
|----------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `region`                   | The region code. _(default: NYC)_ (see [Region API Reference](https://github.com/mikezupper/livepeer-leaderboard-serverless/tree/tasks/livepeer.cloud/proposal2/add-ai-job-support#api-reference)) |
| `jobType`                  | The job type _(default: ai)_. Currently supports `ai`. New Types maybe be added in the future.                                                                                                     |
| `metricsApiEndpoint`       | The URL to the Leaderboard API [post_stats endpoint](https://github.com/mikezupper/livepeer-leaderboard-serverless/tree/tasks/livepeer.cloud/proposal2/add-ai-job-support#api-reference)           |
| `metricsSecret`            | The `SECRET` key used by the Leaderboard API Server.                                                                                                                                               |
| `disableStatsPosting`      | Optional boolean. When `true`, job stats are logged locally instead of being posted to the Leaderboard API. Useful for local debugging.                                                            |
| `broadcasterJobEndpoint`   | The URL to the Livepeer Gateway AI Job Endpoint.                                                                                                                                                   |
| `broadcasterCliEndpoint`   | The URL to the Livepeer Gateway CLI port (typically port 7935). Used exclusively to call `getNetworkCapabilities`, which returns orchestrator addresses, service URIs, and pipeline/model capabilities in a single call.                                                                                           |
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

Orchestrator discovery uses a single `getNetworkCapabilities` call against the Gateway CLI port (`broadcasterCliEndpoint`, default port 7935). This endpoint returns each orchestrator's Ethereum address, service URI (`orch_uri`), and full pipeline/model capability set in one response. No separate orchestrator registry call or `orchMapping` override is needed.

Orchestrators with an empty `orch_uri` in the response are skipped automatically. The tester retries `getNetworkCapabilities` until at least one orchestrator is returned, to handle the Gateway's initial discovery warm-up window.

### Mediamtx Integration

`configs/mediamtx/mediamtx.yml` defines two relevant RTMP paths with dynamic stream key support:

- `~^aiJobTesterStream-[^-]+-.+-[0-9a-f]{8}-[0-9]+$` – Matches ai-job-tester ingest stream IDs and uses `runOnReady` to invoke the Gateway CLI as soon as the tester pushes the input RTMP stream. Update this line to point to the URI of the tester Gateway.
- `~^aiJobTesterStream-[^-]+-.+-[0-9a-f]{8}-[0-9]+-out$` – Records only the plain playback output stream under `recordings/live-video/output/<stream_id>-out/`, allowing you to inspect the final video produced by the orchestrator without also recording request-scoped internal output paths.

MediaMTX is included in the `live` profile in `docker-compose.yml`, so it always starts in the same Compose project as the Gateway. The default Compose project network provides container-to-container name resolution for the `runOnReady` hook automatically — no custom Docker network is needed. The `mediamtx` service bind-mounts [`./recordings`](recordings) to `/app/recordings` inside the container so recordings are visible on the host.

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

The URL fields map like this:

- `liveVideo.mediaServerURL` supplies `rtmp://live-video-to-video-mediamtx:1935`
- `orchestrator=` is the service URI returned by `getNetworkCapabilities` (`orch_uri` field) — no manual override needed. **Omitted when `TEST_INDIVIDUAL_ORCHESTRATORS` is set**, allowing the Gateway to route freely.
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
- Gateway routing fields stay top-level in the RTMP query (`pipeline`, `streamId`, and `orchestrator` when not in individual-orch mode), while inference params are encoded into a single `params=<json>` query field.
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

To run the stack once immediately instead of waiting for the cron schedule, set `RUN_IMMEDIATE=true`. The tester will run once and exit cleanly (exit code 0). Because `restart: unless-stopped` does not restart containers that exit cleanly, the container stops after the single run.

```bash
RUN_IMMEDIATE=true docker compose --profile live up
```

This is the same `live` profile — no separate local profile is needed. Build the image first with `docker build -t ai-job-tester:latest .` if you haven't already.

Config files for the `live` profile are bind-mounted read-only from `./configs/ai-job-tester/` into the container at `/app/configs`. Edit those files directly and restart the stack to pick up changes.

To run a one-off test without Docker, build and invoke the binary directly:

```bash
go build -o jobtester ./cmd/ai-job-tester.go
./jobtester -f configs/ai-job-tester/config-live-video-to-video-pipelines.json
```

Local path and volume requirements:

- `./configs/ai-job-tester/` — config files bind-mounted read-only into the tester container at `/app/configs`.
- `./configs/gateway/` — Gateway credentials bind-mounted into the Gateway container at `/root/.lpData`. Populate with your eth keystore and `eth-secret.txt` before starting. Gitignored.
- `./recordings` — host bind-mount target for MediaMTX recording output. Keep this folder present so the bind mount has a host path to write to.
- No custom Docker network is needed. All services in the same profile share Docker Compose's default project network.

#### Enabling MediaMTX Recording

Recording is disabled by default. To capture the transformed output stream to disk during a local run, edit `configs/mediamtx/mediamtx.yml` and flip `record: no` to `record: yes` on the `-out` path:

```yaml
"~^aiJobTesterStream-[^-]+-.+-[0-9a-f]{8}-[0-9]+-out$":
    record: yes
    recordPath: /app/recordings/live-video/output/%path/%Y-%m-%d_%H-%M-%S
    recordFormat: mpegts
```

Because `mediamtx.yml` is bind-mounted (not baked into the image), you can toggle this without rebuilding. Restart the stack and recordings will appear on the host at `./recordings/live-video/output/<stream_id>-out/<timestamp>.ts`.

For manual inspection using the Docker stack:

1. Edit `configs/ai-job-tester/config-live-video-to-video-pipelines.json` — adjust `liveVideo.testDurationSeconds` for a shorter or longer watch window, and set `disableStatsPosting: true` if you want stats logged locally instead of posted to the leaderboard API.
2. Start the stack with `RUN_IMMEDIATE=true docker compose --profile live up`. This starts MediaMTX, the Gateway, and the tester in one command.
3. The tester runs once and exits. Use the logged `stream_id` or `playback_url` to find the recording. If recording is enabled in `configs/mediamtx/mediamtx.yml`, output streams appear under `recordings/live-video/output/<stream_id>-out/<timestamp>.ts` on the host.
   If no recording directory appears for a prompt, the usual cause is that the live output stream never came online for that prompt, not that MediaMTX failed after recording had already started.
4. Open the playback URL with an external player such as:
   - `ffplay <playback_url>`
   - VLC using the same RTMP/HLS/WebRTC path
5. When you are done, stop the stack with:

```bash
docker compose --profile live down
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

Build the image once before running any `docker compose` profile. The image name must match the `image:` field in `docker-compose.yml` (`ai-job-tester:latest`).

```bash
docker build -t ai-job-tester:latest .
```

Rebuild whenever the source code or `Dockerfile` changes.

## Run the Application

The entire stack is managed through a single [`docker-compose.yml`](docker-compose.yml) with three profiles. Select the profile that matches your intent:

| Profile        | Purpose                                                  | Default Schedule        |
|----------------|----------------------------------------------------------|-------------------------|
| `batch`        | AI batch pipeline testing                                | `*/5 * * * *` (every 5 min) |
| `live`         | Live video-to-video testing (gateway-routed)             | `*/10 * * * *` (every 10 min) |
| `live-network` | Live video-to-video testing (individual orch, no gateway pin) | `50 * * * *` (at :50 each hour) |

```bash
docker compose --profile batch        up -d              # batch (cron, background)
docker compose --profile live         up -d              # live video (cron, background)
docker compose --profile live-network up -d              # live video, individual orch mode (cron, background)
RUN_IMMEDIATE=true docker compose --profile live up      # live video, run once immediately
```

Profiles are mutually exclusive. Do not combine them in the same invocation.

Both profiles use the pre-built `ai-job-tester:latest` image — run `docker build -t ai-job-tester:latest .` from this repo before starting any profile. Set `RUN_IMMEDIATE=true` to run once and exit instead of looping on cron.

### Prerequisites: Local Folders

No external Docker volumes are needed. Config files are provided via bind mounts from this repo.

**`configs/gateway/`** — place the Gateway credential files here before starting any profile:

```
configs/gateway/
  eth-secret.txt          # wallet password
  keystore/UTC--...       # eth keystore file
```

This folder is listed in `.gitignore` so credentials are never committed.

**`configs/ai-job-tester/`** — runtime config JSON files for the tester container. These are already checked in and bind-mounted read-only into the tester at `/app/configs`.

**`./recordings/`** — host bind-mount target for MediaMTX recording output. Keep this folder present so the bind mount has a host path to write to.

No custom Docker network is needed. All services in the same profile share Docker Compose's default project network.

### Job Scheduling

The production profiles use Linux `crontab` inside the container. The schedule is controlled by the `CRONTAB_SCHEDULE` environment variable set in `docker-compose.yml`:

- `batch` profile: `*/5 * * * *` (every 5 minutes)
- `live` profile: `*/10 * * * *` (every 10 minutes)
- `live-network` profile: `50 * * * *` (at 50 minutes past each hour)

**_Note:_** The following Gateway flags are required for the tester Gateway to operate in job-test mode:

Environment Variables:
`LIVEPEER_OS_HTTP_TIMEOUT=8s`
`LIVEPEER_TESTER_GATEWAY_ENABLED=true`

Livepeer Startup Flags:
`-discoveryTimeout=1000ms`
