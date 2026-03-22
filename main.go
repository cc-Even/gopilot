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
		"supervisor",
		sysPrompt,
		os.Getenv("MODEL"),
		agents.WithToolList(agents.DefaultToolDefinitions()),
		//agents.WithSubAgents(map[string]*agents.Agent{
		//	subAgent.Name: subAgent,
		//}),
	)

	msgs := []string{
		"Spawn Alice (coder) and Bob (tester). Send Alice a message:  `tell Bob to report his role to me`, send Alice the exact words.",
		"Broadcast \"status update: phase 1 complete\" to all teammates",
		"Check the lead inbox for any messages",
	}

	for _, msg := range msgs {
		fmt.Println("Input message: ", msg)
		output, err := agent.Run(context.TODO(), msg)
		if err != nil {
			log.Fatal(err)
		}
		if err := agent.TeamManager.WaitUntilIdle(context.TODO()); err != nil {
			log.Fatal(err)
		}

		fmt.Printf("Final output: %s\n", output)
	}
}
