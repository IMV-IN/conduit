from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-conduit-dev")
r = client.chat.completions.create(
    model="auto:cheap",
    messages=[{"role": "user", "content": "Summarize this ticket in one line: …"}],
)
print(r.choices[0].message.content)
print("model:", r.model if hasattr(r, "model") else "?")
