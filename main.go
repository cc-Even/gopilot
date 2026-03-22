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
	//subAgent := agents.NewOpenAIAgent(
	//	"file-agent",
	//	"You are a file reader assistant, use the tools to read and list files.",
	//	os.Getenv("MODEL"),
	//	agents.WithDesc("a files reader and list agent"),
	//	agents.WithToolList(agents.DefaultToolDefinitions()),
	//)
	sysPrompt := fmt.Sprintf(" You are a coding agent at  %s.", WORKDIR)
	agent := agents.NewOpenAIAgent(
		"local-agent",
		sysPrompt,
		os.Getenv("MODEL"),
		agents.WithToolList(agents.DefaultToolDefinitions()),
		//agents.WithSubAgents(map[string]*agents.Agent{
		//	subAgent.Name: subAgent,
		//}),
	)

	msgs := []string{
		`Start 3 background tasks: "sleep 2", "sleep 4", "sleep 6". Check their status.`,
	}

	for _, msg := range msgs {
		output, err := agent.Run(context.TODO(), msg)
		if err != nil {
			log.Fatal(err)
		}

		fmt.Printf("Final output: %s\n", output)
	}
}
