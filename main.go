package main

import (
	"claude-go/agents"
	"context"
	"fmt"
	"log"
	"os"

	"github.com/joho/godotenv"
)

func main() {
	// 加载环境变量
	err := godotenv.Load("setting.env")
	if err != nil {
		log.Println("Warning: No .env file found, using system environment variables")
	}

	subAgent := agents.NewOpenAIAgent(
		"file-agent",
		"You are a file reader assistant, use the tools to read and list files.",
		os.Getenv("MODEL"),
		agents.WithDesc("a files reader and list agent"),
		agents.WithApiKey(os.Getenv("OPENAI_API_KEY")),
		agents.WithToolList(agents.DefaultToolDefinitions()),
	)

	agent := agents.NewOpenAIAgent(
		"local-agent",
		"You are a helpful assistant, use the tools to solve the problem step by step.Use the todo tool to plan multi-step tasks. Mark in_progress before starting, completed when done. summerize the result in the end.",
		os.Getenv("MODEL"),
		agents.WithApiKey(os.Getenv("OPENAI_API_KEY")),
		agents.WithSubAgents(map[string]*agents.OpenAIAgent{
			subAgent.Name: subAgent,
		}),
	)

	output, err := agent.Run(context.TODO(), "Delegate: read all .go files and summarize what each one does")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Final output: %s\n", output)
}
