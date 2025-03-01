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
	"time"

	"github.com/chaisql/chai"
)

// Database models
type Caregiver struct {
	Email            string    `json:"email"`
	Experience       string    `json:"experience"`
	Location         string    `json:"location"`
	Availability     string    `json:"availability"`
	Specializations  string    `json:"specializations"`
	RateExpectations float64   `json:"rate_expectations"`
	Certifications   string    `json:"certifications"`
	CreatedAt        time.Time `json:"created_at"`
}

type Patient struct {
	Email                string    `json:"email"`
	CareNeeds            string    `json:"care_needs"`
	Location             string    `json:"location"`
	ScheduleRequirements string    `json:"schedule_requirements"`
	Budget               float64   `json:"budget"`
	SpecialRequirements  string    `json:"special_requirements"`
	CreatedAt            time.Time `json:"created_at"`
}

type Match struct {
	CaregiverEmail string    `json:"caregiver_email"`
	PatientEmail   string    `json:"patient_email"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

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

type UserContext struct {
	Email string
	Role  string // "caregiver" or "patient"
}

type App struct {
	db          *chai.DB
	messages    []Message
	apiKey      string
	userContext UserContext
}

var (
	chatRoom *App
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

	dbFile = "chat.data"
)

const systemPrompt = `You are a matchmaking assistant helping to connect caregivers with patients. 

IMPORTANT: Before collecting any other information, you must first get the user's email address.
If you don't have their email, ask for it before proceeding with any other questions.

After getting the email, collect and store information using store_caregiver or store_patient functions.

Required information for caregivers:
- Email (MUST BE COLLECTED FIRST)
- Experience and certifications
- Location
- Availability
- Specializations
- Rate expectations (hourly rate in dollars)

Required information for patients:
- Email (MUST BE COLLECTED FIRST)
- Care needs
- Location
- Schedule requirements
- Budget (hourly rate in dollars)
- Special requirements

Always verify you have the email before storing any other information.`

func NewApp(apiKey string) (*App, error) {
	// Initialize database with the specific file
	db, err := chai.Open(dbFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}

	// Create schema
	err = db.Exec(`
		CREATE TABLE IF NOT EXISTS caregivers (
			email TEXT PRIMARY KEY,
			experience TEXT,
			location TEXT,
			availability TEXT,
			specializations TEXT,
			rate_expectations REAL,
			certifications TEXT,
			created_at TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS patients (
			email TEXT PRIMARY KEY,
			care_needs TEXT,
			location TEXT,
			schedule_requirements TEXT,
			budget REAL,
			special_requirements TEXT,
			created_at TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS matches (
			caregiver_email TEXT,
			patient_email TEXT,
			status TEXT,
			created_at TIMESTAMP,
			PRIMARY KEY (caregiver_email, patient_email)
		);
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create schema: %v", err)
	}

	return &App{
		db:          db,
		messages:    make([]Message, 0),
		apiKey:      apiKey,
		userContext: UserContext{},
	}, nil
}

func (app *App) Close() error {
	return app.db.Close()
}

// Database operations
func (app *App) StoreCaregiver(c *Caregiver) error {
	c.CreatedAt = time.Now()
	return app.db.Exec(`
		INSERT INTO caregivers (
			email, experience, location, availability, 
			specializations, rate_expectations, certifications, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, c.Email, c.Experience, c.Location, c.Availability,
		c.Specializations, c.RateExpectations, c.Certifications, c.CreatedAt)
}

func (app *App) StorePatient(p *Patient) error {
	p.CreatedAt = time.Now()
	return app.db.Exec(`
		INSERT INTO patients (
			email, care_needs, location, schedule_requirements,
			budget, special_requirements, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, p.Email, p.CareNeeds, p.Location, p.ScheduleRequirements,
		p.Budget, p.SpecialRequirements, p.CreatedAt)
}

func (app *App) CreateMatch(m *Match) error {
	m.CreatedAt = time.Now()
	return app.db.Exec(`
		INSERT INTO matches (caregiver_email, patient_email, status, created_at)
		VALUES (?, ?, ?, ?)
	`, m.CaregiverEmail, m.PatientEmail, m.Status, m.CreatedAt)
}

func callOpenAI(req ChatRequest) (*ChatResponse, error) {
	functionDefs := []map[string]interface{}{
		{
			"name":        "list_caregivers",
			"description": "List all registered caregivers in the system",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
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
	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", os.Getenv("OPENAI_API_KEY")))

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

		// Check for email in the message if we don't already have one
		if chatRoom.userContext.Email == "" {
			words := strings.Fields(message)
			for _, word := range words {
				if strings.Contains(word, "@") {
					email := strings.Trim(word, ".,!?")
					chatRoom.userContext.Email = email
					log.Printf("Stored email context: %s", email)

					// Store initial caregiver record
					caregiver := &Caregiver{
						Email:     email,
						CreatedAt: time.Now(),
					}
					if err := chatRoom.StoreCaregiver(caregiver); err != nil {
						log.Printf("Error storing initial caregiver record: %v", err)
					}
					break
				}
			}
		}

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
		messages = append(messages, chatRoom.messages...)

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
			case "list_caregivers":
				caregivers, err := chatRoom.ListCaregivers()
				if err != nil {
					log.Printf("Error listing caregivers: %v", err)
				} else {
					response := "Here are the registered caregivers:\n"
					for _, c := range caregivers {
						response += fmt.Sprintf("\nEmail: %s\nLocation: %s\nAvailability: %s\nSpecializations: %s\nRate: $%.2f/hr\n",
							c.Email, c.Location, c.Availability, c.Specializations, c.RateExpectations)
					}
					if err := chatRoom.AddMessage("assistant", response); err != nil {
						log.Printf("Error adding assistant message: %v", err)
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
		messages = chatRoom.messages
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

// AddMessage adds a new message to the app's message history
func (app *App) AddMessage(role, content string) error {
	app.messages = append(app.messages, Message{
		Role:    role,
		Content: content,
	})
	return nil
}

// GetMessagesByRole returns messages filtered by role
func (app *App) GetMessagesByRole(role string) ([]Message, error) {
	var filtered []Message
	for _, msg := range app.messages {
		if msg.Role == role {
			filtered = append(filtered, msg)
		}
	}
	return filtered, nil
}

// ListCaregivers returns all registered caregivers from the database
func (app *App) ListCaregivers() ([]Caregiver, error) {
	var caregivers []Caregiver
	result, err := app.db.Query("SELECT * FROM caregivers")
	if err != nil {
		return nil, fmt.Errorf("failed to query caregivers: %v", err)
	}
	defer result.Close()

	err = result.Iterate(func(r *chai.Row) error {
		var c Caregiver
		if err := r.Scan(&c.Email, &c.Experience, &c.Location, &c.Availability,
			&c.Specializations, &c.RateExpectations, &c.Certifications, &c.CreatedAt); err != nil {
			return fmt.Errorf("failed to scan caregiver: %v", err)
		}
		caregivers = append(caregivers, c)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate caregivers: %v", err)
	}

	return caregivers, nil
}

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY environment variable is required")
	}

	var err error
	chatRoom, err = NewApp(apiKey)
	if err != nil {
		log.Fatal(err)
	}
	defer chatRoom.Close()

	http.HandleFunc("/", handleChat)
	http.HandleFunc("/chat", handleChat)

	port := ":8080"
	fmt.Printf("Server starting on http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
