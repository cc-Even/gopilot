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
	sysPrompt := fmt.Sprintf(" You are a coding agent at  %s. When you are free, check your message", WORKDIR)
	agent := agents.NewOpenAIAgent(
		"supervisor",
		sysPrompt,
		os.Getenv("MODEL"),
		agents.WithToolList(agents.DefaultToolDefinitions()),
	)

	msgs := []string{
		"Create 3 tasks on the board, then spawn alice and bob. Watch them auto-claim.",
		"Spawn a coder teammate and let it find work from the task board itself",
		"Create tasks with dependencies. Watch teammates respect the blocked order.",
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
