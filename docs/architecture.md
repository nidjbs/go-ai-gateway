# Architecture

```text
client -> auth -> alias resolver -> retry/failover -> OpenAI-compatible upstream
                           \-> usage sink (best effort after result)
```

The HTTP gateway is responsible for OpenAI-style request and error envelopes. Routing translates a client-visible alias into an ordered provider list. Retry executes eligible failures on the current provider and then moves to a lower-priority provider. Streaming responses never fail over after their first emitted event.

The runtime intentionally has no database, queue, or warehouse dependency. Authentication and usage reporting are interfaces so deployments can add those concerns without coupling them to proxy execution.
