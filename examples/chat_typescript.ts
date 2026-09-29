import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://localhost:8080/v1",
  apiKey: "sk-conduit-dev",
});

const r = await client.chat.completions.create({
  model: "auto:best",
  messages: [{ role: "user", content: "Write a merge function for two sorted lists." }],
});
console.log(r.choices[0].message.content);
