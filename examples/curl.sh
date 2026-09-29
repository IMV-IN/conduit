curl -s localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-conduit-dev' \
  -d '{
    "model": "auto:cheap",
    "messages": [{"role": "user", "content": "Classify: The delivery was late but the food was excellent. One word."}],
    "max_tokens": 32
  }' | jq '{content: .choices[0].message.content}'

# Decision-only sidecar (no provider call):
curl -s localhost:8080/v1/route \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-conduit-dev' \
  -d '{"model": "auto:best", "messages": [{"role": "user", "content": "solve 3x^2 - 12x + 9 = 0"}]}' | jq .
