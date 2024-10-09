# livepeer-job-tester

## Overview

This application is responsible for executing AI Test Jobs against each Livepeer Orchestrator on the Livpeeer AI Network.

### Key Features Support
* Each Orchestrator is only tested for the Pipelines and Models they support
* Configuration-based support for multiple pipelines and models. 
  * Easily add new Pipeline and Model support over time.
* Integrates with the Livepeer Leaderboard Serverless API (TODO add link)
* Decoupled from go-livepeer Gateway Node.
  * Uses the HTTP Rest Endpoint to send jobs to the gateway node.
* Docker Supported
  * Enables `Crontab` scheduling of Test Jobs 

### Figure 1 - Logical Architecture

This repository covers the *"Livepeer AI Job Tester"* box in the overall logical architecture.

![Job Tester Logical Architecture](docs/logical_architecture.png)

## Setup Environments

The setup requires docker

The setup requires a running Gateway (see self-hosted tutorial)
    - the gateway is a "special fork" go-livepeer (to enable single orch tests)
    - include the branch (with the hopes livepeer fixes the selection algo)

- How does the tester know what models to send to a given orch?
- How is the job tester scheduled and run on intervals?

## Docker

- how to build the docker image
- how to use the docker images
- Only Support docker :)


### Development

How to setup up locally and test 

### Production

How to deploy docker image via docker compose


## Configuration File

TODO: list all configs and describe them
- How to add new pipelines?
- How to configure a Stats API server?
- How to configure a Gateway
- how to configure the test's ip/port for Webhook URL?

## API Endpoints

- /orchestrators 
  - used for the Gateway to find "the orch to test"

## AI Test Job Inputs
TODO: 
- how are pipelines, models, orchs located?
- what config file entries are used for each pipelline?
- what JSON inputs are used for each pipeline?
- What assets are used when submitting an AI Job to livepeer? (audio, image to image file, upscale file, etc ????)

# Stats JSON

- what data will be sent to the Stats API endpoint?
- How do enhance this JSON going forward? (new pipeline gets added)
- What are the possible error codes?
- What does "Round Trip TIme" mean?






