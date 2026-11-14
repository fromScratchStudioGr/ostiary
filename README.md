<p align="center">
  <img src="docs/ostiary-logo.png#gh-light-mode-only" alt="Ostiary" width="420">
  <img src="docs/ostiary-logo-on-dark.png#gh-dark-mode-only" alt="Ostiary" width="420">
</p>

<p align="center">
  <img src="https://img.shields.io/badge/coverage-96.6%25-brightgreen" alt="Test coverage: 96.6%">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="License: MIT"></a>
</p>

**Ostiary** is a **fast, simple and secure gateway** between your applications and AI model providers such as OpenAI, Anthropic, Google Gemini and DeepSeek. Your applications talk to Ostiary with keys of their own. Ostiary **holds the real provider keys**, sends each request on, and **records what every request costs**.

It is an **LLM gateway and only that**. **You run it, you control it, you own it.**

## Why we built it

We wanted an LLM gateway we could use ourselves: really fast, easy to scale and secure. It had to be simple to set up and simple to use, without all the extras that usually come with it. No upselling, no paid tier, no strings attached.

We also wanted it to be as open as possible. Ostiary works with open source tools for keeping keys safe, and with open models. When you need them, it works just as well with Google Cloud and with closed models.

The idea is simple: control everything, own everything. If you want an LLM gateway and only that, Ostiary is for you.

Four goals guided every decision.

**Fast.** Ostiary adds well under a millisecond of its own time to a typical request before it reaches the provider. Answers stream back to your applications as they arrive.

**Scalable.** A basic machine handles well over a thousand requests per second. Every copy of Ostiary shares the same database and cache, so you can run more copies side by side when you need more.

**Easy to use.** It starts with one command. Your applications keep using the official OpenAI and Anthropic libraries. You only change the address and the key.

**Secure.** Provider keys are stored encrypted and are only unlocked for the moment a request needs them. Your applications never see them. Each application gets its own key, which you can limit or switch off at any time.

## The name

The word ostiary comes from the Latin ostiarius, the doorkeeper who decided who could pass through a door. Ostiary does the same for your AI providers: it checks every key at the door and keeps the real keys safely inside.

## What it does

- Gives each application, team or person its own Ostiary key, so the real provider keys stay in one place.
- Keeps provider keys encrypted with a key service of your choice: OpenBao, Google Cloud KMS, or a built in local option for trying it out.
- Records the tokens and the cost of every request, using each model's own prices, cached input included.
- Lets you set a spending limit for each application on each provider key.
- Streams answers back as the provider sends them.
- Speaks the providers' own formats: OpenAI chat completions, Anthropic messages and Gemini interactions.
- Comes with an [admin panel](https://github.com/fromScratchStudioGr/ostiary-panel) for keys, applications and usage.
- Publishes metrics for Prometheus, so you can monitor it 24/7.

## Performance

Measured on a basic virtual machine with 8 CPUs and 16 GB of RAM, under a steady load of **1,400 requests per second** for 30 minutes.

| Measure | Result |
|---|---|
| Errors | **0** |
| Data | 6.5 GB received, **140 GB sent back** |
| Tokens | **1,241,811 output** and 727,246 input per second |

Time from receiving a request to sending it to the provider:

| Median | Average | 95th percentile | 99th percentile |
|---|---|---|---|
| **411 µs** | 2 ms | 10 ms | 19 ms |

The [documentation](https://fromscratchstudiogr.github.io/ostiary-docs/) covers how these were measured, in more detail.

## Getting started

You need Docker.

1. Get the code.

   ```
   git clone https://github.com/fromScratchStudioGr/ostiary.git
   cd ostiary
   ```

2. Create a `.env` file with a master key for the local key service.

   ```
   echo "LOCAL_KMS_MASTER_KEY=$(openssl rand -base64 32)" > .env
   ```

   The local key service is meant for trying Ostiary out. For real use, set up OpenBao or Google Cloud KMS as the documentation describes.

3. Start Ostiary.

   ```
   docker compose up -d
   ```

4. Open the admin panel at http://localhost:3001 and sign in as `admin` with the password `admin`. Change the password straight away.

5. In the panel, add a provider key and create a consumer for your application. Give the consumer access to the provider key, with a spending limit if you like, then create an Ostiary key for it. Ostiary keys start with `ost_`.

6. Send your first request.

   ```
   curl http://localhost:8080/v1/chat/completions \
     -H "Authorization: Bearer ost_your_key" \
     -H "Content-Type: application/json" \
     -d '{"model": "gpt-5.4-mini", "messages": [{"role": "user", "content": "Hello"}]}'
   ```

To use the official libraries instead, point them at Ostiary and give them your Ostiary key. The OpenAI library takes `http://localhost:8080/v1` as its base URL, and the Anthropic library takes `http://localhost:8080`.

## Documentation

The [documentation](https://fromscratchstudiogr.github.io/ostiary-docs/) covers configuration, key management, deployment, the supported models and the API.

## Status

Ostiary is at version 1 and stable.

## Contributing

Contributions are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) explains how to run Ostiary and its tests locally.

## Security

If you find a security issue, please [report it privately](https://github.com/fromScratchStudioGr/ostiary/security/advisories/new) instead of opening a public issue. Only you and the maintainers can see the report.

## License

Ostiary is released under the [MIT License](LICENSE).
