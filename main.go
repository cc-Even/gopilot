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
	WORKDIR, _ := os.Getwd()
	subAgent := agents.NewOpenAIAgent(
		"file-agent",
		"You are a file reader assistant, use the tools to read and list files.",
		os.Getenv("MODEL"),
		agents.WithDesc("a files reader and list agent"),
		agents.WithToolList(agents.DefaultToolDefinitions()),
	)
	sysPrompt := fmt.Sprintf(" You are a coding agent at  %s.", WORKDIR)
	agent := agents.NewOpenAIAgent(
		"local-agent",
		sysPrompt,
		os.Getenv("MODEL"),
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
