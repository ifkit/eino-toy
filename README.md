# eino-toy

Terminal AI chat (default API endpoint as for LM Studio:`localhost:1234`). Built on [eino](https://github.com/cloudwego/eino) and Bubble Tea v2.

```
go run . --openai https://api.anthropic.com/v1 --model claude-sonnet-5-5 --key <your_key>
```

Use an ACP agent instead (the agent must speak [ACP](https://agentclientprotocol.com) over stdio; tool permission requests are denied):

```
go run . --acp "npx @zed-industries/claude-code-acp"
```
