# Architecture

```text
client -> auth -> alias resolver -> adapter capability filter -> retry/failover -> provider adapter -> upstream protocol
                                                 \-> usage sink (best effort after result)
```

The public HTTP gateway is OpenAI-compatible. Routing translates a client-visible alias into an ordered provider list; adapter capability filtering removes providers that cannot satisfy the requested operation before retry/failover begins. `openai` providers retain raw JSON forwarding with only the resolved-model rewrite, while `anthropic` providers translate the supported Chat Completions subset to the Messages API and map responses back to OpenAI envelopes.

Retry executes eligible failures on the current provider and then moves to a lower-priority provider. Streaming responses never fail over after their first emitted client-visible event.

The runtime intentionally has no database, queue, or warehouse dependency. Authentication and usage reporting are interfaces so deployments can add those concerns without coupling them to proxy execution.
