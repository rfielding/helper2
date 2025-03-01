package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sync"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type ChatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

type ChatRoom struct {
	Messages []Message
	mu       sync.Mutex
}

var (
	chatRoom = &ChatRoom{
		Messages: make([]Message, 0),
	}
	apiKey string
)

const htmlTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Chat Room</title>
    <style>
        .chat-container {
            max-width: 800px;
            margin: 0 auto;
            padding: 20px;
        }
        .message {
            margin: 10px 0;
            padding: 10px;
            border-radius: 5px;
        }
        .user {
            background-color: #e3f2fd;
        }
        .assistant {
            background-color: #f5f5f5;
        }
        .system {
            background-color: #fff3e0;
        }
    </style>
</head>
<body>
    <div class="chat-container">
        <h1>Chat Room</h1>
        <div id="messages">
            {{range .Messages}}
            <div class="message {{.Role}}">
                <strong>{{.Role}}:</strong> {{.Content}}
            </div>
            {{end}}
        </div>
        <form method="POST" action="/chat">
            <input type="text" name="message" placeholder="Type your message..." style="width: 80%">
            <button type="submit">Send</button>
        </form>
    </div>
</body>
</html>
`

func (cr *ChatRoom) AddMessage(role, content string) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.Messages = append(cr.Messages, Message{Role: role, Content: content})
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		message := r.FormValue("message")
		if message == "" {
			http.Error(w, "Message cannot be empty", http.StatusBadRequest)
			return
		}

		chatRoom.AddMessage("user", message)

		// Prepare the OpenAI API request
		messages := append([]Message{}, chatRoom.Messages...)
		chatReq := ChatRequest{
			Model:    "gpt-3.5-turbo",
			Messages: messages,
		}

		jsonData, err := json.Marshal(chatReq)
		if err != nil {
			http.Error(w, "Failed to marshal request", http.StatusInternalServerError)
			return
		}

		// Make the API call to OpenAI
		req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewBuffer(jsonData))
		if err != nil {
			http.Error(w, "Failed to create request", http.StatusInternalServerError)
			return
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "Failed to make API request", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		var chatResp ChatResponse
		if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
			http.Error(w, "Failed to decode API response", http.StatusInternalServerError)
			return
		}

		if len(chatResp.Choices) > 0 {
			chatRoom.AddMessage("assistant", chatResp.Choices[0].Message.Content)
		}

		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	tmpl, err := template.New("chat").Parse(htmlTemplate)
	if err != nil {
		http.Error(w, "Failed to parse template", http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, chatRoom)
	if err != nil {
		http.Error(w, "Failed to execute template", http.StatusInternalServerError)
		return
	}
}

func main() {
	apiKey = os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY environment variable is required")
	}

	http.HandleFunc("/", handleChat)
	http.HandleFunc("/chat", handleChat)

	port := ":8080"
	fmt.Printf("Server starting on http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
