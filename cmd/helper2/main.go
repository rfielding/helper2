package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ichiban/prolog"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

// Simplify to just use string arrays for arguments
type FunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Choice struct {
	Message struct {
		Role         string        `json:"role"`
		Content      string        `json:"content"`
		FunctionCall *FunctionCall `json:"function_call,omitempty"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type ChatResponse struct {
	Choices []Choice `json:"choices"`
}

type ChatRoom struct {
	Messages []Message
	mu       sync.Mutex
	pl       *prolog.Interpreter
}

var (
	chatRoom = &ChatRoom{
		Messages: make([]Message, 0),
		pl:       prolog.New(nil, nil),
	}
	apiKey string
)

const (
	htmlTemplate = `
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
            <select name="role">
                <option value="user">User</option>
                <option value="admin">Admin</option>
            </select>
            <input type="text" name="message" placeholder="Type your message..." style="width: 80%">
            <button type="submit">Send</button>
        </form>
    </div>
</body>
</html>
`
	dbFile = "facts.pl" // File to store Prolog facts
)

const systemPrompt = `You are a matchmaking assistant helping to connect caregivers with patients. 

IMPORTANT: Before collecting any other information, you must first get the user's email address.
If you don't have their email, ask for it before proceeding with any other questions.

After getting the email, for each piece of information:
1. Use the add_fact function to store it
2. Acknowledge what was stored
3. Ask follow-up questions about missing information

Required information for caregivers:
- Email (MUST BE COLLECTED FIRST)
- Experience and certifications
- Location
- Availability
- Specializations
- Rate expectations

Required information for patients:
- Email (MUST BE COLLECTED FIRST)
- Care needs
- Location
- Schedule requirements
- Budget
- Special requirements

Always verify you have the email before storing any other information.`

// Add email to fact structure in Prolog
func (cr *ChatRoom) initProlog() error {
	err := cr.pl.Exec(`
		:- dynamic(fact/4).  % Email, Category, Fact, Timestamp
		
		% Find facts by email and category
		find_facts(Email, Category, Facts) :-
			findall(Fact, fact(Email, Category, Fact, _), Facts).
		
		% Find matching facts by email, category and pattern
		find_matching_facts(Email, Category, Pattern, Facts) :-
			findall(Fact, (
				fact(Email, Category, Fact, _),
				sub_string(Fact, _, _, _, Pattern)
			), Facts).

		% Check if email exists
		has_email(Email) :-
			fact(Email, 'email', _, _).
	`)
	if err != nil {
		return fmt.Errorf("failed to initialize prolog: %v", err)
	}
	return nil
}

// Helper function to sanitize strings for Prolog atoms
func sanitizeForProlog(s string) string {
	// Replace @ with _at_
	s = strings.ReplaceAll(s, "@", "_at_")
	// Replace . with _dot_
	s = strings.ReplaceAll(s, ".", "_dot_")
	// Replace spaces with underscores
	s = strings.ReplaceAll(s, " ", "_")
	// Remove any other special characters
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, s)
	return s
}

// Modify AddFact to handle Prolog atom sanitization
func (cr *ChatRoom) AddFact(email, category, fact string) (string, error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	log.Printf("Adding fact - Email: %s, Category: %s, Fact: %s", email, category, fact)

	// Sanitize email and category for use as Prolog atoms
	safeEmail := sanitizeForProlog(email)
	safeCategory := sanitizeForProlog(category)

	// Format the Prolog fact - use single quotes to preserve the original fact string
	// Remove any trailing dot from the fact before adding our own
	fact = strings.TrimSuffix(fact, ".")
	prologFact := fmt.Sprintf("fact('%s', '%s', %s).",
		safeEmail,
		safeCategory,
		fact, // Use the fact directly as it's already in Prolog format
	)

	log.Printf("Asserting Prolog fact: %s", prologFact)

	// Assert the fact
	err := cr.pl.Exec(`assert(` + prologFact + `)`)
	if err != nil {
		log.Printf("Error asserting fact: %v", err)
		return "", fmt.Errorf("failed to assert fact: %v", err)
	}

	// Log current facts for this email
	sols, err := cr.pl.Query(`find_facts(?, _, Facts).`, safeEmail)
	if err == nil {
		defer sols.Close()
		log.Printf("Current facts for email %s:", email)
		for sols.Next() {
			var s struct{ Facts []string }
			if err := sols.Scan(&s); err == nil {
				for _, fact := range s.Facts {
					log.Printf("  %s", fact)
				}
			}
		}
	}

	return prologFact, nil
}

// Add function to check if email exists
func (cr *ChatRoom) HasEmail(email string) bool {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	sols, err := cr.pl.Query(`has_email(?).`, email)
	if err != nil {
		return false
	}
	defer sols.Close()

	return sols.Next()
}

// Add method to save facts to disk
func (cr *ChatRoom) saveFacts() error {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	var facts []string
	sols, err := cr.pl.Query(`listing(fact/3).`)
	if err != nil {
		return fmt.Errorf("failed to query facts: %v", err)
	}
	defer sols.Close()

	for sols.Next() {
		var s struct {
			Listing string
		}
		if err := sols.Scan(&s); err != nil {
			return fmt.Errorf("failed to scan facts: %v", err)
		}
		facts = append(facts, s.Listing)
	}

	return os.WriteFile(dbFile, []byte(strings.Join(facts, "\n")), 0644)
}

// Add method to extract facts from message
func (cr *ChatRoom) ExtractFacts(message string) error {
	sols, err := cr.pl.Query(`extract_facts(?, Facts).`, message)
	if err != nil {
		return fmt.Errorf("failed to extract facts: %v", err)
	}
	defer sols.Close()

	for sols.Next() {
		var s struct {
			Facts []string
		}
		if err := sols.Scan(&s); err != nil {
			return fmt.Errorf("failed to scan facts: %v", err)
		}
		for _, fact := range s.Facts {
			_, err := cr.AddFact("auto", "extracted", fact)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (cr *ChatRoom) AddMessage(role, content string) error {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	msg := Message{Role: role, Content: content}
	cr.Messages = append(cr.Messages, msg)

	// Add message to Prolog database
	err := cr.pl.Exec(`assert(message(?, ?, ?)).`,
		role,              // Role
		content,           // Content
		time.Now().Unix(), // Timestamp
	)
	if err != nil {
		return fmt.Errorf("failed to assert message: %v", err)
	}

	// If it's an assistant message, try to extract facts
	if role == "assistant" {
		err = cr.pl.Exec(`
			(is_fact(?, Fact) ->
				assert(fact(Fact, ?))
			; true).
		`, content, time.Now().Unix())
		if err != nil {
			return fmt.Errorf("failed to assert fact: %v", err)
		}
	}

	return nil
}

// Modify QueryFacts to support categories
func (cr *ChatRoom) QueryFacts(category, query string) ([]string, error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	var facts []string
	sols, err := cr.pl.Query(`find_matching_facts(?, ?, Facts).`, category, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query facts: %v", err)
	}
	defer sols.Close()

	for sols.Next() {
		var s struct {
			Facts []string
		}
		if err := sols.Scan(&s); err != nil {
			return nil, fmt.Errorf("failed to scan solution: %v", err)
		}
		facts = append(facts, s.Facts...)
	}

	return facts, nil
}

// Get all messages for a specific role
func (cr *ChatRoom) GetMessagesByRole(role string) ([]Message, error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	var messages []Message
	sols, err := cr.pl.Query(`message(?, Content, _).`, role)
	if err != nil {
		return nil, fmt.Errorf("failed to query messages: %v", err)
	}
	defer sols.Close()

	for sols.Next() {
		var s struct {
			Content string
		}
		if err := sols.Scan(&s); err != nil {
			return nil, fmt.Errorf("failed to scan solution: %v", err)
		}
		messages = append(messages, Message{Role: role, Content: s.Content})
	}

	return messages, nil
}

func callOpenAI(req ChatRequest) (*ChatResponse, error) {
	functionDefs := []map[string]interface{}{
		{
			"name":        "add_fact",
			"description": "Add one or more Prolog facts to the database. Each argument should be a complete Prolog fact string like: caregiver(email, phone) or availability(email, time).",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"arguments": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
						},
						"minItems": 1,
					},
				},
				"required": []string{"arguments"},
			},
		},
		{
			"name":        "query_facts",
			"description": "Query facts from the database using Prolog patterns",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"category": map[string]interface{}{
						"type":        "string",
						"description": "Category to query",
					},
					"pattern": map[string]interface{}{
						"type":        "string",
						"description": "Pattern to match",
					},
				},
				"required": []string{"category", "pattern"},
			},
		},
	}

	requestBody := map[string]interface{}{
		"model":     req.Model,
		"messages":  req.Messages,
		"functions": functionDefs,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %v", err)
	}

	// Log the request being sent to OpenAI
	log.Printf("Sending request to OpenAI:\n%s", prettyPrint(jsonData))

	// Make the API call to OpenAI
	request, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	resp, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("failed to make API request: %v", err)
	}
	defer resp.Body.Close()

	// Log the response from OpenAI
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewBuffer(respBody))
	log.Printf("Response from OpenAI:\n%s", prettyPrint(respBody))

	var chatResp ChatResponse
	if err := json.NewDecoder(bytes.NewBuffer(respBody)).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("failed to decode API response: %v", err)
	}

	return &chatResp, nil
}

// Helper function to pretty print JSON
func prettyPrint(b []byte) string {
	var out bytes.Buffer
	err := json.Indent(&out, b, "", "  ")
	if err != nil {
		return string(b)
	}
	return out.String()
}

// Modify the GetArguments method to handle both string array and object formats
func (fc *FunctionCall) GetArguments() ([]string, error) {
	// First unmarshal the string-encoded JSON
	var jsonStr string
	if err := json.Unmarshal(fc.Arguments, &jsonStr); err == nil {
		// Then try to unmarshal the inner JSON string
		var argsWrapper struct {
			Arguments []string `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &argsWrapper); err == nil {
			return argsWrapper.Arguments, nil
		}
		log.Printf("Failed to parse inner JSON: %v", err)
	}

	// If the above fails, log the raw arguments for debugging
	log.Printf("Raw arguments received: %s", string(fc.Arguments))
	return nil, fmt.Errorf("failed to parse arguments: %v", string(fc.Arguments))
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		message := r.FormValue("message")
		role := r.FormValue("role")

		log.Printf("\n=== New Chat Message ===")
		log.Printf("Role: %s", role)
		log.Printf("Message: %s", message)

		if message == "" {
			http.Error(w, "Message cannot be empty", http.StatusBadRequest)
			return
		}

		// Add user message
		if err := chatRoom.AddMessage("user", message); err != nil {
			log.Printf("Error adding message: %v", err)
			http.Error(w, "Failed to add message", http.StatusInternalServerError)
			return
		}

		// Prepare messages with system prompt
		messages := []Message{
			{
				Role:    "system",
				Content: systemPrompt,
			},
		}
		messages = append(messages, chatRoom.Messages...)

		chatReq := ChatRequest{
			Model:    "gpt-3.5-turbo",
			Messages: messages,
		}

		chatResp, err := callOpenAI(chatReq)
		if err != nil {
			log.Printf("Error calling OpenAI: %v", err)
			http.Error(w, "Failed to call OpenAI", http.StatusInternalServerError)
			return
		}

		// Handle function calls
		if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message.FunctionCall != nil {
			fc := chatResp.Choices[0].Message.FunctionCall
			args, err := fc.GetArguments()
			if err != nil {
				log.Printf("Error parsing function arguments: %v", err)
				http.Error(w, "Failed to parse function arguments", http.StatusInternalServerError)
				return
			}

			log.Printf("\nFunction call received - Name: %s", fc.Name)
			log.Printf("Arguments: %v", args)

			switch fc.Name {
			case "add_fact":
				if len(args) > 0 {
					// Parse the Prolog fact directly
					fact := args[0]
					// Extract email from the fact (assuming it's the first argument in the fact)
					email := ""
					if start := strings.Index(fact, "'"); start != -1 {
						if end := strings.Index(fact[start+1:], "'"); end != -1 {
							email = fact[start+1 : start+1+end]
						}
					}

					log.Printf("Extracted email: %s from fact: %s", email, fact)

					if email == "" {
						response := "I need your email address before I can store any information. Could you please provide your email?"
						if err := chatRoom.AddMessage("assistant", response); err != nil {
							log.Printf("Error adding assistant message: %v", err)
						}
					} else {
						// Store the entire fact
						prologFact, err := chatRoom.AddFact(email, "fact", fact)
						if err != nil {
							log.Printf("Error adding fact: %v", err)
						} else {
							log.Printf("Added Prolog fact: %s", prologFact)
							response := "I've stored your information. Is there anything else you'd like to add about your experience or certifications?"
							if err := chatRoom.AddMessage("assistant", response); err != nil {
								log.Printf("Error adding assistant message: %v", err)
							}
						}
					}
				}
			case "query_facts":
				if len(args) >= 2 {
					category := args[0]
					query := args[1]

					log.Printf("Querying facts - Category: %s, Query: %s", category, query)
					facts, err := chatRoom.QueryFacts(category, query)
					if err != nil {
						log.Printf("Error querying facts: %v", err)
					} else {
						log.Printf("Found facts: %v", facts)
						messages = append(messages, Message{
							Role:    "function",
							Content: fmt.Sprintf("Found facts: %v", facts),
						})
					}
				}
			}
		} else if len(chatResp.Choices) > 0 {
			assistantResponse := chatResp.Choices[0].Message.Content
			if assistantResponse == "" {
				assistantResponse = "I need your email address before we proceed. Could you please provide it?"
			}
			log.Printf("\nAssistant response: %s", assistantResponse)

			if err := chatRoom.AddMessage("assistant", assistantResponse); err != nil {
				log.Printf("Error adding assistant message: %v", err)
				http.Error(w, "Failed to add assistant message", http.StatusInternalServerError)
				return
			}
		}

		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Add a query parameter to filter by role
	role := r.URL.Query().Get("role")
	var messages []Message
	var err error

	if role != "" {
		messages, err = chatRoom.GetMessagesByRole(role)
		if err != nil {
			http.Error(w, "Failed to get messages", http.StatusInternalServerError)
			return
		}
	} else {
		messages = chatRoom.Messages
	}

	data := struct {
		Messages []Message
		Role     string
	}{
		Messages: messages,
		Role:     role,
	}

	tmpl, err := template.New("chat").Parse(htmlTemplate)
	if err != nil {
		http.Error(w, "Failed to parse template", http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, data)
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

	// Initialize Prolog database
	if err := chatRoom.initProlog(); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", handleChat)
	http.HandleFunc("/chat", handleChat)

	port := ":8080"
	fmt.Printf("Server starting on http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
