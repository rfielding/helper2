package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chaisql/chai"
)

// Database models
type Caregiver struct {
	Email            string    `json:"email"`
	Name             string    `json:"name"`
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
	Name                 string    `json:"name"`
	CareNeeds            string    `json:"care_needs"`
	Location             string    `json:"location"`
	ScheduleRequirements string    `json:"schedule_requirements"`
	Budget               float64   `json:"budget"`
	SpecialRequirements  string    `json:"special_requirements"`
	PhoneNumber          string    `json:"phone_number"`
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
	db           *chai.DB
	userSessions map[string][]Message // Map of email -> messages
	apiKey       string
	maxHistory   int
	mu           sync.RWMutex // Mutex for thread-safe access
}

var (
	chatRoom *App
)

const (
	htmlTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Helper - Connecting Caregivers to Patients</title>
    <style>
        .header {
            text-align: center;
            margin-bottom: 20px;
        }
        .red-cross {
            color: #FF0000;
            font-size: 2em;
            margin-bottom: 10px;
        }
        .app-description {
            color: #666;
            font-style: italic;
            margin-bottom: 20px;
        }
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
        .message-form {
            display: flex;
            gap: 10px;
            margin-top: 20px;
        }
        .message-input {
            flex-grow: 1;
            padding: 8px;
            border: 1px solid #ddd;
            border-radius: 4px;
        }
        .send-button {
            padding: 8px 16px;
            background-color: #4CAF50;
            color: white;
            border: none;
            border-radius: 4px;
            cursor: pointer;
        }
        .send-button:hover {
            background-color: #45a049;
        }
        .avatar {
            width: 40px;
            height: 40px;
            border-radius: 50%;
            object-fit: cover;
            margin-right: 10px;
            vertical-align: middle;
        }
        .user-email {
            text-align: right;
            color: #666;
            margin-bottom: 10px;
            display: flex;
            align-items: center;
            justify-content: flex-end;
            gap: 10px;
        }
        .matches-list {
            list-style: none;
            padding: 0;
            margin: 0;
        }
        .match-item {
            background: #ffffff;
            border: 1px solid #e0e0e0;
            border-radius: 8px;
            padding: 15px;
            margin: 10px 0;
            box-shadow: 0 2px 4px rgba(0,0,0,0.05);
            display: flex;
            align-items: center;
            gap: 15px;
        }
        .match-avatar {
            width: 60px;
            height: 60px;
            border-radius: 50%;
            object-fit: cover;
        }
        .match-details {
            flex-grow: 1;
        }
        .match-details span {
            display: block;
            margin: 5px 0;
            color: #666;
        }
        .match-details strong {
            color: #333;
            font-size: 1.1em;
        }
    </style>
</head>
<body>
    <div class="chat-container">
        <div class="header">
            <div class="red-cross">✚</div>
            <h1>Helper</h1>
            <div class="app-description">Connecting Caregivers to Patients</div>
        </div>
        <div class="user-email">
            <img src="static/images/default-avatar.png" alt="User Avatar" class="avatar">
            Logged in as: {{.UserEmail}}
        </div>
        <div id="messages">
            {{range .Messages}}
            <div class="message {{.Role}}">
                <strong>{{.Role}}:</strong> {{.Content | safeHTML}}
            </div>
            {{end}}
        </div>
        <form method="POST" action="chat" class="message-form">
            <input type="hidden" name="email" value="{{.UserEmail}}">
            <input type="text" name="message" placeholder="Type your message..." class="message-input" required>
            <button type="submit" class="send-button">Send</button>
        </form>
    </div>
</body>
</html>
`

	dbFile = "chat.data"
)

const systemPrompt = `You are a matchmaking assistant helping to connect caregivers with patients. 

When a user connects, their email is already provided in the URL or form data. Use this email as their identifier 
and DO NOT ask for it again. You can see it in their messages history.

For new users, first determine if they are a caregiver or patient by asking about their role.

Required information for caregivers:
- Experience and certifications
- Location
- Availability
- Specializations
- Rate expectations (hourly rate in dollars)

Required information for patients:
- Care needs
- Location
- Schedule requirements
- Budget (hourly rate in dollars)
- Special requirements

Once you have collected all required information:
- For caregivers: Confirm their registration and offer to show matching patients
- For patients: Show them matching caregivers immediately

Always maintain context from previous messages to avoid asking for information that was already provided.
`

// Add these new types and methods

type QueryFilter struct {
	Field    string
	Operator string
	Value    interface{}
}

type DynamicQuery struct {
	Table   string
	Fields  []string
	Filters []QueryFilter
	OrderBy string
	Limit   int
}

// BuildDynamicQuery safely constructs a parameterized SQL query
func (app *App) BuildDynamicQuery(q DynamicQuery) (string, []interface{}, error) {
	// Validate table name against whitelist
	allowedTables := map[string]bool{
		"caregivers": true,
		"patients":   true,
		"matches":    true,
		"skills":     true,
	}
	if !allowedTables[q.Table] {
		return "", nil, fmt.Errorf("invalid table name: %s", q.Table)
	}

	// Validate field names against whitelist
	allowedFields := map[string]bool{
		"email":                 true,
		"experience":            true,
		"location":              true,
		"availability":          true,
		"specializations":       true,
		"rate_expectations":     true,
		"certifications":        true,
		"created_at":            true,
		"care_needs":            true,
		"schedule_requirements": true,
		"budget":                true,
		"special_requirements":  true,
		"status":                true,
		"skill":                 true,
	}

	// Build SELECT clause
	selectFields := "*"
	if len(q.Fields) > 0 {
		validFields := make([]string, 0)
		for _, f := range q.Fields {
			if allowedFields[f] {
				validFields = append(validFields, f)
			}
		}
		if len(validFields) > 0 {
			selectFields = strings.Join(validFields, ", ")
		}
	}

	// Build WHERE clause and params
	var whereConditions []string
	var params []interface{}
	allowedOperators := map[string]bool{
		"=":           true,
		">":           true,
		"<":           true,
		">=":          true,
		"<=":          true,
		"LIKE":        true,
		"NOT LIKE":    true,
		"IN":          true,
		"NOT IN":      true,
		"IS NULL":     true,
		"IS NOT NULL": true,
	}

	for _, filter := range q.Filters {
		if !allowedFields[filter.Field] || !allowedOperators[filter.Operator] {
			continue
		}

		switch filter.Operator {
		case "IS NULL", "IS NOT NULL":
			whereConditions = append(whereConditions,
				fmt.Sprintf("%s %s", filter.Field, filter.Operator))
		default:
			whereConditions = append(whereConditions,
				fmt.Sprintf("%s %s ?", filter.Field, filter.Operator))
			params = append(params, filter.Value)
		}
	}

	// Construct final query
	query := fmt.Sprintf("SELECT %s FROM %s", selectFields, q.Table)
	if len(whereConditions) > 0 {
		query += " WHERE " + strings.Join(whereConditions, " AND ")
	}
	if q.OrderBy != "" && allowedFields[strings.TrimSuffix(q.OrderBy, " DESC")] {
		query += " ORDER BY " + q.OrderBy
	}
	if q.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", q.Limit)
	}

	return query, params, nil
}

// ExecuteDynamicQuery executes a dynamic query and returns results
func (app *App) ExecuteDynamicQuery(q DynamicQuery) ([]map[string]interface{}, error) {
	query, params, err := app.BuildDynamicQuery(q)
	if err != nil {
		return nil, err
	}

	result, err := app.db.Query(query, params...)
	if err != nil {
		return nil, fmt.Errorf("failed to execute query: %v", err)
	}
	defer result.Close()

	var results []map[string]interface{}
	err = result.Iterate(func(r *chai.Row) error {
		// Get column names
		cols, err := r.Columns()
		if err != nil {
			return err
		}

		// Create a slice of interface{} to hold the values
		values := make([]interface{}, len(cols))
		valuePtrs := make([]interface{}, len(cols))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		// Scan the result into the values
		if err := r.Scan(valuePtrs...); err != nil {
			return err
		}

		// Create a map for this row
		row := make(map[string]interface{})
		for i, col := range cols {
			row[col] = values[i]
		}
		results = append(results, row)
		return nil
	})

	return results, err
}

// Fix the dynamicQueryFunction definition
var dynamicQueryFunction = map[string]interface{}{
	"name":        "execute_dynamic_query",
	"description": "Execute a dynamic database query with filters",
	"parameters": map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"table": map[string]interface{}{
				"type": "string",
				"enum": []string{"caregivers", "patients", "matches"},
			},
			"fields": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
			"filters": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"field":    map[string]interface{}{"type": "string"},
						"operator": map[string]interface{}{"type": "string"},
						"value":    map[string]interface{}{"type": "string"},
					},
				},
			},
			"order_by": map[string]interface{}{"type": "string"},
			"limit":    map[string]interface{}{"type": "integer"},
		},
		"required": []string{"table"},
	},
}

// Add new type for Skills
type Skill struct {
	Email     string    `json:"email"`
	Skill     string    `json:"skill"`
	CreatedAt time.Time `json:"created_at"`
}

func NewApp(apiKey string) (*App, error) {
	db, err := chai.Open(dbFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}

	// Update schema with IF NOT EXISTS for indexes
	err = db.Exec(`
		CREATE TABLE IF NOT EXISTS caregivers (
			email TEXT PRIMARY KEY,
			name TEXT,
			experience TEXT,
			location TEXT,
			availability TEXT,
			specializations TEXT,
			rate_expectations REAL,
			certifications TEXT,
			created_at TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_caregivers_email ON caregivers(email);

		CREATE TABLE IF NOT EXISTS patients (
			email TEXT PRIMARY KEY,
			name TEXT,
			care_needs TEXT,
			location TEXT,
			schedule_requirements TEXT,
			budget REAL,
			special_requirements TEXT,
			phone_number TEXT,
			created_at TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_patients_email ON patients(email);

		CREATE TABLE IF NOT EXISTS matches (
			caregiver_email TEXT,
			patient_email TEXT,
			status TEXT,
			created_at TIMESTAMP,
			PRIMARY KEY (caregiver_email, patient_email)
		);
		CREATE INDEX IF NOT EXISTS idx_matches_caregiver_email ON matches(caregiver_email);
		CREATE INDEX IF NOT EXISTS idx_matches_patient_email ON matches(patient_email);

		CREATE TABLE IF NOT EXISTS chat_history (
			email TEXT,
			role TEXT,
			content TEXT,
			created_at TIMESTAMP,
			recipient TEXT,
			PRIMARY KEY (email, created_at)
		);
		CREATE INDEX IF NOT EXISTS idx_chat_history_email ON chat_history(email);

		CREATE TABLE IF NOT EXISTS skills (
			email TEXT,
			skill TEXT,
			created_at TIMESTAMP,
			PRIMARY KEY (email, skill)
		);
		CREATE INDEX IF NOT EXISTS idx_skills_email ON skills(email)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create schema: %v", err)
	}

	return &App{
		db:           db,
		userSessions: make(map[string][]Message),
		apiKey:       apiKey,
		maxHistory:   100,
	}, nil
}

func (app *App) Close() error {
	return app.db.Close()
}

// Database operations
func (app *App) StoreCaregiver(c *Caregiver) error {
	c.CreatedAt = time.Now()

	// Check if caregiver exists
	result, err := app.db.Query("SELECT email FROM caregivers WHERE email = ?", c.Email)
	if err != nil {
		return fmt.Errorf("failed to check caregiver existence: %v", err)
	}
	defer result.Close()

	exists := false
	err = result.Iterate(func(r *chai.Row) error {
		exists = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to iterate results: %v", err)
	}

	if exists {
		// Update existing caregiver
		return app.db.Exec(`
			UPDATE caregivers 
			SET name = ?,
				experience = ?,
				location = ?,
				availability = ?,
				specializations = ?,
				rate_expectations = ?,
				certifications = ?
			WHERE email = ?
		`, c.Name, c.Experience, c.Location, c.Availability,
			c.Specializations, c.RateExpectations, c.Certifications,
			c.Email)
	}

	// Insert new caregiver
	return app.db.Exec(`
		INSERT INTO caregivers (
			email, name, experience, location, availability, 
			specializations, rate_expectations, certifications, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, c.Email, c.Name, c.Experience, c.Location, c.Availability,
		c.Specializations, c.RateExpectations, c.Certifications, c.CreatedAt)
}

func (app *App) StorePatient(p *Patient) error {
	p.CreatedAt = time.Now()

	// Check if patient exists
	result, err := app.db.Query("SELECT email FROM patients WHERE email = ?", p.Email)
	if err != nil {
		return fmt.Errorf("failed to check patient existence: %v", err)
	}
	defer result.Close()

	exists := false
	err = result.Iterate(func(r *chai.Row) error {
		exists = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to iterate results: %v", err)
	}

	if exists {
		// Update existing patient
		return app.db.Exec(`
			UPDATE patients 
			SET name = ?,
				care_needs = ?,
				location = ?,
				schedule_requirements = ?,
				budget = ?,
				special_requirements = ?,
				phone_number = ?
			WHERE email = ?
		`, p.Name, p.CareNeeds, p.Location, p.ScheduleRequirements,
			p.Budget, p.SpecialRequirements, p.PhoneNumber,
			p.Email)
	}

	// Insert new patient
	return app.db.Exec(`
		INSERT INTO patients (
			email, name, care_needs, location, schedule_requirements,
			budget, special_requirements, phone_number, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.Email, p.Name, p.CareNeeds, p.Location, p.ScheduleRequirements,
		p.Budget, p.SpecialRequirements, p.PhoneNumber, p.CreatedAt)
}

func (app *App) CreateMatch(m *Match) error {
	m.CreatedAt = time.Now()
	return app.db.Exec(`
		INSERT INTO matches (caregiver_email, patient_email, status, created_at)
		VALUES (?, ?, ?, ?)
	`, m.CaregiverEmail, m.PatientEmail, m.Status, m.CreatedAt)
}

func callOpenAI(req ChatRequest) (*ChatResponse, error) {
	// Add logging before API call
	log.Printf("Calling OpenAI API...")

	functionDefs := []map[string]interface{}{
		{
			"name":        "store_caregiver",
			"description": "Store a new caregiver's information in the system",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"email": map[string]interface{}{
						"type":        "string",
						"description": "Caregiver's email address",
					},
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Caregiver's full name",
					},
					"experience": map[string]interface{}{
						"type":        "string",
						"description": "Years of experience and certifications",
					},
					"location": map[string]interface{}{
						"type":        "string",
						"description": "Caregiver's location",
					},
					"availability": map[string]interface{}{
						"type":        "string",
						"description": "Availability schedule",
					},
					"specializations": map[string]interface{}{
						"type":        "string",
						"description": "Areas of specialization",
					},
					"rate_expectations": map[string]interface{}{
						"type":        "number",
						"description": "Hourly rate in dollars",
					},
					"certifications": map[string]interface{}{
						"type":        "string",
						"description": "Professional certifications",
					},
				},
				"required": []string{"email", "name", "location", "rate_expectations"},
			},
		},
		{
			"name":        "store_patient",
			"description": "Store a new patient's information in the system",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"email": map[string]interface{}{
						"type":        "string",
						"description": "Patient's email address",
					},
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Patient's full name",
					},
					"care_needs": map[string]interface{}{
						"type":        "string",
						"description": "Description of care needs",
					},
					"location": map[string]interface{}{
						"type":        "string",
						"description": "Patient's location",
					},
					"schedule_requirements": map[string]interface{}{
						"type":        "string",
						"description": "Schedule requirements",
					},
					"budget": map[string]interface{}{
						"type":        "number",
						"description": "Hourly budget in dollars",
					},
					"special_requirements": map[string]interface{}{
						"type":        "string",
						"description": "Any special requirements",
					},
				},
				"required": []string{"email", "name", "care_needs", "location"},
			},
		},
		{
			"name":        "list_patients",
			"description": "List all registered patients in the system",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			"name":        "list_caregivers",
			"description": "List all registered caregivers in the system",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			"name":        "find_matching_caregivers",
			"description": "Find caregivers matching a patient's requirements",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"patient_email": map[string]interface{}{
						"type":        "string",
						"description": "Email of the patient seeking care",
					},
				},
				"required": []string{"patient_email"},
			},
		},
		dynamicQueryFunction,
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
	log.Printf("Sending request to OpenAI...")

	// Make the API call to OpenAI
	request, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", os.Getenv("OPENAI_API_KEY")))

	// Add timeout to the client
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	log.Printf("Waiting for OpenAI response...")
	resp, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("failed to make API request: %v", err)
	}
	defer resp.Body.Close()

	log.Printf("Received response from OpenAI")

	// Read the response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}

	var chatResp ChatResponse
	if err := json.NewDecoder(bytes.NewBuffer(respBody)).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("failed to decode API response: %v", err)
	}

	return &chatResp, nil
}

// Update the GetArguments method to handle the direct JSON object format
func (fc *FunctionCall) GetArguments() (map[string]interface{}, error) {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(fc.Arguments), &args); err != nil {
		// Try parsing as a string first
		var strArgs string
		if err := json.Unmarshal(fc.Arguments, &strArgs); err != nil {
			return nil, fmt.Errorf("failed to parse arguments: %v", err)
		}
		// Then parse the string as JSON
		if err := json.Unmarshal([]byte(strArgs), &args); err != nil {
			return nil, fmt.Errorf("failed to parse string arguments: %v", err)
		}
	}
	return args, nil
}

// Update the data structure passed to the template
type TemplateData struct {
	Messages  []Message
	UserEmail string
}

// Update handleChat function to include user email
func handleChat(w http.ResponseWriter, r *http.Request) {
	userEmail := r.URL.Query().Get("email")
	if userEmail == "" {
		userEmail = r.FormValue("email")
	}

	if r.Method == "POST" {
		message := r.FormValue("message")
		if message == "" {
			http.Error(w, "Message cannot be empty", http.StatusBadRequest)
			return
		}

		log.Printf("Processing message from %s: %s", userEmail, message)

		// Add user's message to chat history
		if err := chatRoom.AddMessageWithRecipient(userEmail, "user", message, "admin"); err != nil {
			log.Printf("Error adding message: %v", err)
			http.Error(w, "Failed to add message", http.StatusInternalServerError)
			return
		}

		// Get chat history for OpenAI
		messages := []Message{
			{Role: "system", Content: systemPrompt},
		}
		messages = append(messages, chatRoom.GetUserMessages(userEmail)...)

		// Call OpenAI
		chatReq := ChatRequest{
			Model:    "gpt-3.5-turbo",
			Messages: messages,
		}

		chatResp, err := callOpenAI(chatReq)
		if err != nil {
			log.Printf("Error calling OpenAI: %v", err)
			http.Error(w, "Failed to process message", http.StatusInternalServerError)
			return
		}

		// Process OpenAI response
		if err := handleOpenAIResponse(chatResp, userEmail, chatRoom); err != nil {
			log.Printf("Error handling OpenAI response: %v", err)
			http.Error(w, "Failed to process OpenAI response", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, fmt.Sprintf("./?email=%s", url.QueryEscape(userEmail)), http.StatusSeeOther)
		return
	}

	// Handle GET request
	log.Printf("Getting messages for email: %s", userEmail)
	messages := chatRoom.GetUserMessages(userEmail)

	data := TemplateData{
		Messages:  messages,
		UserEmail: userEmail,
	}

	// Create template with the safeHTML function
	tmpl, err := template.New("chat").Funcs(template.FuncMap{
		"safeHTML": func(s string) template.HTML {
			return template.HTML(s)
		},
	}).Parse(htmlTemplate)
	if err != nil {
		http.Error(w, "Failed to parse template", http.StatusInternalServerError)
		return
	}

	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, "Failed to execute template", http.StatusInternalServerError)
		return
	}
}

// Helper functions to safely get values from the arguments map
func getStringArg(args map[string]interface{}, key string, defaultValue string) string {
	if val, ok := args[key]; ok && val != nil {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return defaultValue
}

func getFloatArg(args map[string]interface{}, key string, defaultValue float64) float64 {
	if val, ok := args[key]; ok && val != nil {
		switch v := val.(type) {
		case float64:
			return v
		case float32:
			return float64(v)
		case int:
			return float64(v)
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return defaultValue
}

// AddMessage adds a new message to the app's message history
func (app *App) AddMessage(email, role, content string) error {
	return app.AddMessageWithRecipient(email, role, content, "admin")
}

// GetMessagesByRole returns messages filtered by role for a specific user
func (app *App) GetMessagesByRole(email, role string) ([]Message, error) {
	app.mu.RLock()
	defer app.mu.RUnlock()

	var filtered []Message
	if messages, exists := app.userSessions[email]; exists {
		for _, msg := range messages {
			if msg.Role == role {
				filtered = append(filtered, msg)
			}
		}
	}
	return filtered, nil
}

// ListPatients returns all patients from the database
func (app *App) ListPatients() ([]Patient, error) {
	var patients []Patient
	result, err := app.db.Query("SELECT * FROM patients")
	if err != nil {
		return nil, fmt.Errorf("failed to query patients: %v", err)
	}
	defer result.Close()

	err = result.Iterate(func(r *chai.Row) error {
		var p Patient
		if err := r.Scan(&p.Email, &p.Name, &p.CareNeeds, &p.Location,
			&p.ScheduleRequirements, &p.Budget, &p.SpecialRequirements, &p.PhoneNumber, &p.CreatedAt); err != nil {
			return fmt.Errorf("failed to scan patient: %v", err)
		}
		patients = append(patients, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate patients: %v", err)
	}

	return patients, nil
}

// ListCaregivers returns all caregivers from the database
func (app *App) ListCaregivers() ([]Caregiver, error) {
	var caregivers []Caregiver
	result, err := app.db.Query("SELECT * FROM caregivers")
	if err != nil {
		return nil, fmt.Errorf("failed to query caregivers: %v", err)
	}
	defer result.Close()

	err = result.Iterate(func(r *chai.Row) error {
		var c Caregiver
		if err := r.Scan(&c.Email, &c.Name, &c.Experience, &c.Location,
			&c.Availability, &c.Specializations, &c.RateExpectations, &c.Certifications, &c.CreatedAt); err != nil {
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

// FindMatchingCaregivers finds caregivers that match a patient's requirements
func (app *App) FindMatchingCaregivers(patientEmail string) ([]Caregiver, error) {
	// First get the patient's requirements
	var patient Patient
	result, err := app.db.Query("SELECT * FROM patients WHERE email = ?", patientEmail)
	if err != nil {
		return nil, fmt.Errorf("failed to query patient: %v", err)
	}
	defer result.Close()

	found := false
	err = result.Iterate(func(r *chai.Row) error {
		if err := r.Scan(&patient.Email, &patient.Name, &patient.CareNeeds, &patient.Location,
			&patient.ScheduleRequirements, &patient.Budget, &patient.SpecialRequirements,
			&patient.PhoneNumber, &patient.CreatedAt); err != nil {
			return fmt.Errorf("failed to scan patient: %v", err)
		}
		found = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("patient not found")
	}

	log.Printf("Finding caregivers for patient %s (Location: %s, Budget: $%.2f)",
		patient.Email, patient.Location, patient.Budget)

	// Fixed query syntax by removing the CASE statement
	result, err = app.db.Query(`
		SELECT * FROM caregivers 
		WHERE LOWER(location) LIKE LOWER(?) 
		AND rate_expectations <= ?
		ORDER BY 
			rate_expectations ASC
	`, "%"+patient.Location+"%", patient.Budget)
	if err != nil {
		return nil, fmt.Errorf("failed to query matching caregivers: %v", err)
	}
	defer result.Close()

	var caregivers []Caregiver
	err = result.Iterate(func(r *chai.Row) error {
		var c Caregiver
		if err := r.Scan(&c.Email, &c.Name, &c.Experience, &c.Location,
			&c.Availability, &c.Specializations, &c.RateExpectations, &c.Certifications, &c.CreatedAt); err != nil {
			return fmt.Errorf("failed to scan caregiver: %v", err)
		}
		caregivers = append(caregivers, c)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate matching caregivers: %v", err)
	}

	return caregivers, nil
}

// FindMatchingPatients finds patients that match a caregiver's criteria
func (app *App) FindMatchingPatients(caregiverEmail string) ([]Patient, error) {
	// First get the caregiver's details
	var caregiver Caregiver
	result, err := app.db.Query("SELECT * FROM caregivers WHERE email = ?", caregiverEmail)
	if err != nil {
		return nil, fmt.Errorf("failed to query caregiver: %v", err)
	}
	defer result.Close()

	found := false
	err = result.Iterate(func(r *chai.Row) error {
		if err := r.Scan(&caregiver.Email, &caregiver.Name, &caregiver.Experience,
			&caregiver.Location, &caregiver.Availability, &caregiver.Specializations,
			&caregiver.RateExpectations, &caregiver.Certifications, &caregiver.CreatedAt); err != nil {
			return fmt.Errorf("failed to scan caregiver: %v", err)
		}
		found = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("caregiver not found")
	}

	log.Printf("Finding patients for caregiver %s (Location: %s, Rate: $%.2f)",
		caregiver.Email, caregiver.Location, caregiver.RateExpectations)

	// Fixed query syntax by removing the CASE statement
	result, err = app.db.Query(`
		SELECT * FROM patients 
		WHERE LOWER(location) LIKE LOWER(?) 
		AND budget >= ?
		ORDER BY 
			budget DESC
	`, "%"+caregiver.Location+"%", caregiver.RateExpectations)
	if err != nil {
		return nil, fmt.Errorf("failed to query matching patients: %v", err)
	}
	defer result.Close()

	var patients []Patient
	err = result.Iterate(func(r *chai.Row) error {
		var p Patient
		if err := r.Scan(&p.Email, &p.Name, &p.CareNeeds, &p.Location,
			&p.ScheduleRequirements, &p.Budget, &p.SpecialRequirements, &p.PhoneNumber, &p.CreatedAt); err != nil {
			return fmt.Errorf("failed to scan patient: %v", err)
		}
		patients = append(patients, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate matching patients: %v", err)
	}

	return patients, nil
}

// Add new method to load chat history
func (app *App) LoadChatHistory(email string) ([]Message, error) {
	var messages []Message

	result, err := app.db.Query(`
		SELECT role, content 
		FROM chat_history 
		WHERE email = ? 
		ORDER BY created_at DESC 
		LIMIT ?
	`, email, app.maxHistory)
	if err != nil {
		return nil, fmt.Errorf("failed to query chat history: %v", err)
	}
	defer result.Close()

	err = result.Iterate(func(r *chai.Row) error {
		var msg Message
		if err := r.Scan(&msg.Role, &msg.Content); err != nil {
			return fmt.Errorf("failed to scan message: %v", err)
		}
		messages = append(messages, msg)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate chat history: %v", err)
	}

	// Reverse the messages so they're in chronological order
	for i := len(messages)/2 - 1; i >= 0; i-- {
		opp := len(messages) - 1 - i
		messages[i], messages[opp] = messages[opp], messages[i]
	}

	return messages, nil
}

// Add methods to manage skills
func (app *App) AddSkill(email, skill string) error {
	return app.db.Exec(`
		INSERT INTO skills (email, skill, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT (email, skill) DO NOTHING
	`, email, skill, time.Now())
}

func (app *App) GetSkills(email string) ([]string, error) {
	var skills []string
	result, err := app.db.Query("SELECT skill FROM skills WHERE email = ?", email)
	if err != nil {
		return nil, fmt.Errorf("failed to query skills: %v", err)
	}
	defer result.Close()

	err = result.Iterate(func(r *chai.Row) error {
		var skill string
		if err := r.Scan(&skill); err != nil {
			return fmt.Errorf("failed to scan skill: %v", err)
		}
		skills = append(skills, skill)
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to iterate skills: %v", err)
	}

	return skills, nil
}

func (app *App) RemoveSkill(email, skill string) error {
	return app.db.Exec("DELETE FROM skills WHERE email = ? AND skill = ?", email, skill)
}

// GetUserMessages gets all messages for a specific email from the database
func (app *App) GetUserMessages(email string) []Message {
	var messages []Message
	result, err := app.db.Query(`
		SELECT role, content 
		FROM chat_history 
		WHERE email = ? 
		ORDER BY created_at ASC
	`, email)
	if err != nil {
		log.Printf("Error querying chat history for %s: %v", email, err)
		return messages
	}
	defer result.Close()

	err = result.Iterate(func(r *chai.Row) error {
		var msg Message
		if err := r.Scan(&msg.Role, &msg.Content); err != nil {
			log.Printf("Error scanning message: %v", err)
			return err
		}
		messages = append(messages, msg)
		return nil
	})
	if err != nil {
		log.Printf("Error iterating chat history: %v", err)
		return messages
	}

	return messages
}

// AddMessageWithRecipient adds a message to the chat history
func (app *App) AddMessageWithRecipient(email, role, content, recipient string) error {
	// Store in database
	err := app.db.Exec(`
		INSERT INTO chat_history (
			email, role, content, recipient, created_at
		) VALUES (?, ?, ?, ?, ?)
	`, email, role, content, recipient, time.Now())

	if err != nil {
		return fmt.Errorf("failed to store message: %v", err)
	}

	return nil
}

// Add this debug function
func (app *App) DebugPrintAllMessages() {
	result, err := app.db.Query("SELECT email, role, content, created_at FROM chat_history ORDER BY email, created_at")
	if err != nil {
		log.Printf("Debug: Error querying all messages: %v", err)
		return
	}
	defer result.Close()

	log.Println("Debug: All messages in database:")
	err = result.Iterate(func(r *chai.Row) error {
		var email, role, content string
		var createdAt time.Time
		if err := r.Scan(&email, &role, &content, &createdAt); err != nil {
			return err
		}
		log.Printf("Email: %s, Role: %s, Time: %s, Content: %s",
			email, role, createdAt.Format(time.RFC3339), content)
		return nil
	})
	if err != nil {
		log.Printf("Debug: Error iterating messages: %v", err)
	}
}

// Add this type to represent a chat message
type ChatMessage struct {
	Email   string
	Message string
}

// Simplified processTestData that processes messages sequentially
func processTestData(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open test data file: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			log.Printf("Skipping invalid line format: %s", line)
			continue
		}

		email, message := parts[0], parts[1]
		log.Printf("Processing message from %s: %s", email, message)

		// Add user message
		if err := chatRoom.AddMessageWithRecipient(email, "user", message, "admin"); err != nil {
			log.Printf("Error adding message for %s: %v", email, err)
			continue
		}

		// Get chat history and process with OpenAI
		messages := []Message{
			{Role: "system", Content: systemPrompt},
		}
		messages = append(messages, chatRoom.GetUserMessages(email)...)

		// Process with OpenAI
		chatReq := ChatRequest{
			Model:    "gpt-3.5-turbo",
			Messages: messages,
		}

		resp, err := callOpenAI(chatReq)
		if err != nil {
			log.Printf("Error calling OpenAI for %s: %v", email, err)
			continue
		}

		// Handle OpenAI response
		if err := handleOpenAIResponse(resp, email, chatRoom); err != nil {
			log.Printf("Error handling OpenAI response for %s: %v", email, err)
			continue
		}

		log.Printf("Completed processing message for %s", email)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading test data: %v", err)
	}

	return nil
}

// Update formatPatientList to include phone numbers for caregivers
func formatPatientList(patients []Patient) string {
	var sb strings.Builder

	if len(patients) == 0 {
		return "<p>No matching patients found.</p>"
	}

	sb.WriteString("<h3>Matching Patients</h3>")
	sb.WriteString("<ul class='matches-list'>")

	for _, p := range patients {
		// Get skills for this patient
		skills, err := chatRoom.GetSkills(p.Email)
		if err != nil {
			log.Printf("Error getting skills for patient %s: %v", p.Email, err)
			skills = []string{} // Use empty list if error
		}

		sb.WriteString("<li class='match-item'>")
		sb.WriteString("<img src='static/images/default-avatar.png' alt='Patient Avatar' class='match-avatar'>")
		sb.WriteString("<div class='match-details'>")
		sb.WriteString(fmt.Sprintf("<strong>%s</strong><br>", p.Name))
		sb.WriteString(fmt.Sprintf("<span>✉️ Email: %s</span><br>", p.Email))
		sb.WriteString(fmt.Sprintf("<span>📍 Location: %s</span><br>", p.Location))
		sb.WriteString(fmt.Sprintf("<span>💰 Budget: $%.2f/hour</span><br>", p.Budget))
		sb.WriteString(fmt.Sprintf("<span>🕒 Schedule: %s</span><br>", p.ScheduleRequirements))
		sb.WriteString(fmt.Sprintf("<span>ℹ️ Care Needs: %s</span><br>", p.CareNeeds))
		sb.WriteString(fmt.Sprintf("<span>📱 Contact: %s</span><br>", p.PhoneNumber))

		if len(skills) > 0 {
			sb.WriteString("<span>🎯 Skills needed: ")
			for i, skill := range skills {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(skill)
			}
			sb.WriteString("</span>")
		}
		sb.WriteString("</div></li>")
	}

	sb.WriteString("</ul>")
	return sb.String()
}

func formatCaregiverList(caregivers []Caregiver) string {
	var sb strings.Builder

	if len(caregivers) == 0 {
		return "<p>No matching caregivers found.</p>"
	}

	sb.WriteString("<h3>Matching Caregivers</h3>")
	sb.WriteString("<ul class='matches-list'>")

	for _, c := range caregivers {
		// Get skills for this caregiver
		skills, err := chatRoom.GetSkills(c.Email)
		if err != nil {
			log.Printf("Error getting skills for caregiver %s: %v", c.Email, err)
			skills = []string{} // Use empty list if error
		}

		sb.WriteString("<li class='match-item'>")
		sb.WriteString("<img src='static/images/default-avatar.png' class='match-avatar'>")
		sb.WriteString("<div class='match-details'>")
		sb.WriteString(fmt.Sprintf("<strong>%s</strong><br>", c.Name))
		sb.WriteString(fmt.Sprintf("<span>✉️ Email: %s</span><br>", c.Email))
		sb.WriteString(fmt.Sprintf("<span>📍 Location: %s</span><br>", c.Location))
		sb.WriteString(fmt.Sprintf("<span>💰 Rate: $%.2f/hour</span><br>", c.RateExpectations))
		sb.WriteString(fmt.Sprintf("<span>🕒 Availability: %s</span><br>", c.Availability))
		sb.WriteString(fmt.Sprintf("<span>📚 Experience: %s</span><br>", c.Experience))
		sb.WriteString(fmt.Sprintf("<span>🎓 Certifications: %s</span><br>", c.Certifications))
		if len(skills) > 0 {
			sb.WriteString("<span>🎯 Skills: ")
			for i, skill := range skills {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(skill)
			}
			sb.WriteString("</span>")
		}
		sb.WriteString("</div></li>")
	}

	sb.WriteString("</ul>")
	return sb.String()
}

func handleOpenAIResponse(resp *ChatResponse, email string, app *App) error {
	if len(resp.Choices) == 0 {
		return nil
	}

	choice := resp.Choices[0].Message
	if choice.FunctionCall != nil {
		args, err := choice.FunctionCall.GetArguments()
		if err != nil {
			return fmt.Errorf("error parsing function arguments: %v", err)
		}

		var response string
		switch choice.FunctionCall.Name {
		case "list_patients":
			patients, err := app.ListPatients()
			if err != nil {
				response = fmt.Sprintf("Error listing patients: %v", err)
			} else {
				response = formatPatientList(patients)
			}

		case "list_caregivers":
			caregivers, err := app.ListCaregivers()
			if err != nil {
				response = fmt.Sprintf("Error listing caregivers: %v", err)
			} else {
				response = formatCaregiverList(caregivers)
			}

		case "find_matching_caregivers":
			caregivers, err := app.FindMatchingCaregivers(email)
			if err != nil {
				response = fmt.Sprintf("Error finding matches: %v", err)
			} else {
				response = formatCaregiverList(caregivers)
			}

		case "find_matching_patients":
			patients, err := app.FindMatchingPatients(email)
			if err != nil {
				response = fmt.Sprintf("Error finding matches: %v", err)
			} else {
				response = formatPatientList(patients)
			}

		case "store_caregiver":
			caregiver := &Caregiver{
				Email:            email, // Use current user's email
				Name:             getStringArg(args, "name", ""),
				Experience:       getStringArg(args, "experience", ""),
				Location:         getStringArg(args, "location", ""),
				Availability:     getStringArg(args, "availability", ""),
				Specializations:  getStringArg(args, "specializations", ""),
				RateExpectations: getFloatArg(args, "rate_expectations", 0),
				Certifications:   getStringArg(args, "certifications", ""),
			}
			if err := app.StoreCaregiver(caregiver); err != nil {
				response = fmt.Sprintf("Error storing caregiver: %v", err)
			} else {
				response = "Successfully registered as a caregiver."
			}

		case "store_patient":
			patient := &Patient{
				Email:                email, // Use current user's email
				Name:                 getStringArg(args, "name", ""),
				CareNeeds:            getStringArg(args, "care_needs", ""),
				Location:             getStringArg(args, "location", ""),
				ScheduleRequirements: getStringArg(args, "schedule_requirements", ""),
				Budget:               getFloatArg(args, "budget", 0),
				SpecialRequirements:  getStringArg(args, "special_requirements", ""),
				PhoneNumber:          getStringArg(args, "phone_number", ""),
				CreatedAt:            time.Now(),
			}
			if err := app.StorePatient(patient); err != nil {
				response = fmt.Sprintf("Error storing patient: %v", err)
			} else {
				response = "Successfully registered as a patient."
			}
		}

		if response != "" {
			if err := app.AddMessageWithRecipient(email, "assistant", response, "admin"); err != nil {
				return fmt.Errorf("error adding function response: %v", err)
			}
		}
	}

	if choice.Content != "" {
		if err := app.AddMessageWithRecipient(email, "assistant", choice.Content, "admin"); err != nil {
			return fmt.Errorf("error adding assistant response: %v", err)
		}
	}

	return nil
}

// Add this function to test all matches
func testAllMatches(app *App) {
	log.Println("\n=== Testing All Matches ===")

	// Get all caregivers and patients
	caregivers, err := app.ListCaregivers()
	if err != nil {
		log.Printf("Error listing caregivers: %v", err)
		return
	}

	patients, err := app.ListPatients()
	if err != nil {
		log.Printf("Error listing patients: %v", err)
		return
	}

	// Show all patient -> caregiver matches
	log.Println("\nPatient -> Caregiver Matches:")
	for _, p := range patients {
		log.Printf("\nPatient: %s", p.Email)
		log.Printf("Matches with all caregivers:")
		for _, c := range caregivers {
			log.Printf("  • %s", c.Email)
		}
	}

	// Show all caregiver -> patient matches
	log.Println("\nCaregiver -> Patient Matches:")
	for _, c := range caregivers {
		log.Printf("\nCaregiver: %s", c.Email)
		log.Printf("Matches with all patients:")
		for _, p := range patients {
			log.Printf("  • %s", p.Email)
		}
	}
}

func (app *App) handlePatientRegistration(email string, messages []Message) error {
	// Extract patient information from messages
	patient := &Patient{
		Email:                email,
		Name:                 extractName(messages),
		CareNeeds:            extractCareNeeds(messages),
		Location:             extractLocation(messages),
		ScheduleRequirements: extractSchedule(messages),
		Budget:               extractBudget(messages),
		SpecialRequirements:  extractSpecialRequirements(messages),
		PhoneNumber:          extractPhoneNumber(messages),
		CreatedAt:            time.Now(),
	}

	// Store the patient
	return app.StorePatient(patient)
}

// Helper functions to extract information from messages
func extractName(messages []Message) string {
	for _, msg := range messages {
		if strings.Contains(strings.ToLower(msg.Content), "i'm") {
			parts := strings.Split(msg.Content, "i'm ")
			if len(parts) > 1 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func extractCareNeeds(messages []Message) string {
	for _, msg := range messages {
		if strings.Contains(strings.ToLower(msg.Content), "elderly care") ||
			strings.Contains(strings.ToLower(msg.Content), "care needs") {
			return msg.Content
		}
	}
	return ""
}

func extractLocation(messages []Message) string {
	for _, msg := range messages {
		if strings.Contains(strings.ToLower(msg.Content), "located in") {
			parts := strings.Split(msg.Content, "located in ")
			if len(parts) > 1 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func extractSchedule(messages []Message) string {
	for _, msg := range messages {
		if strings.Contains(strings.ToLower(msg.Content), "full-time") ||
			strings.Contains(strings.ToLower(msg.Content), "part-time") ||
			strings.Contains(strings.ToLower(msg.Content), "schedule") {
			return msg.Content
		}
	}
	return ""
}

func extractBudget(messages []Message) float64 {
	for _, msg := range messages {
		if strings.Contains(strings.ToLower(msg.Content), "budget") ||
			strings.Contains(strings.ToLower(msg.Content), "$") {
			// Extract number after $ sign
			parts := strings.Split(msg.Content, "$")
			if len(parts) > 1 {
				numStr := strings.Split(parts[1], "/")[0]
				if budget, err := strconv.ParseFloat(numStr, 64); err == nil {
					return budget
				}
			}
		}
	}
	return 0
}

func extractSpecialRequirements(messages []Message) string {
	for _, msg := range messages {
		if strings.Contains(strings.ToLower(msg.Content), "must have") ||
			strings.Contains(strings.ToLower(msg.Content), "require") {
			return msg.Content
		}
	}
	return ""
}

func extractPhoneNumber(messages []Message) string {
	for _, msg := range messages {
		if strings.Contains(strings.ToLower(msg.Content), "phone") {
			parts := strings.Split(msg.Content, "phone: ")
			if len(parts) > 1 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// Add this function to check if messages indicate patient registration
func isPatientRegistration(messages []Message) bool {
	for _, msg := range messages {
		content := strings.ToLower(msg.Content)
		if strings.Contains(content, "register") && strings.Contains(content, "patient") {
			return true
		}
	}
	return false
}

// Modify the hasAllRequiredInfo function to remove unused variable
func hasAllRequiredInfo(messages []Message) bool {
	var (
		hasCareNeeds bool
		hasLocation  bool
		hasSchedule  bool
		hasBudget    bool
	)

	for _, msg := range messages {
		content := strings.ToLower(msg.Content)

		if strings.Contains(content, "elderly care") ||
			strings.Contains(content, "care needs") {
			hasCareNeeds = true
		}
		if strings.Contains(content, "located in") {
			hasLocation = true
		}
		if strings.Contains(content, "full-time") ||
			strings.Contains(content, "part-time") ||
			strings.Contains(content, "schedule") {
			hasSchedule = true
		}
		if strings.Contains(content, "$") ||
			strings.Contains(content, "budget") {
			hasBudget = true
		}
	}

	return hasCareNeeds && hasLocation && hasSchedule && hasBudget
}

var loadTest = flag.Bool("test", false, "Load test data on startup")

func main() {
	flag.Parse()
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY environment variable is required")
	}

	var err error
	// Fix: Assign to global chatRoom variable
	chatRoom, err = NewApp(apiKey)
	if err != nil {
		log.Fatal(err)
	}
	defer chatRoom.Close()

	// Serve static files before other routes
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	http.HandleFunc("/", handleChat)
	http.HandleFunc("/chat", handleChat)

	// Process test data if the file exists
	go func() {
		if *loadTest {
			if _, err := os.Stat("testdata.txt"); err == nil {
				log.Println("Processing test data...")
				if err := processTestData("testdata.txt"); err != nil {
					log.Printf("Error processing test data: %v", err)
				}
				log.Println("Completed processing test data")

				// Run matching tests after processing test data
				testAllMatches(chatRoom)
			}
		}
	}()

	port := ":8080"
	fmt.Printf("Server starting on http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}

func (app *App) handleChat(email string, message string) (string, error) {
	// Add message to history
	if err := app.AddMessageWithRecipient(email, "user", message, "system"); err != nil {
		return "", err
	}

	messages := app.GetUserMessages(email)

	// Check if this is a patient registration flow and all info is provided
	if isPatientRegistration(messages) && hasAllRequiredInfo(messages) {
		// Register the patient
		patient := &Patient{
			Email:                email,
			Name:                 extractName(messages),
			CareNeeds:            extractCareNeeds(messages),
			Location:             extractLocation(messages),
			ScheduleRequirements: extractSchedule(messages),
			Budget:               extractBudget(messages),
			SpecialRequirements:  extractSpecialRequirements(messages),
			PhoneNumber:          extractPhoneNumber(messages),
			CreatedAt:            time.Now(),
		}

		if err := app.StorePatient(patient); err != nil {
			return "", fmt.Errorf("failed to store patient: %v", err)
		}

		// After registration, show matching caregivers
		caregivers, err := app.FindMatchingCaregivers(email)
		if err != nil {
			return "", fmt.Errorf("failed to find matches: %v", err)
		}
		return formatCaregiverList(caregivers), nil
	}

	// Handle match command
	if strings.ToLower(message) == "match" || strings.ToLower(message) == "list matches" {
		caregivers, err := app.FindMatchingCaregivers(email)
		if err != nil {
			return "", fmt.Errorf("failed to find matches: %v", err)
		}
		return formatCaregiverList(caregivers), nil
	}

	// Rest of chat handling...
	return "", nil
}

// Add helper function to check if user is a caregiver
func (app *App) IsCaregiver(email string) bool {
	result, err := app.db.Query("SELECT 1 FROM caregivers WHERE email = ?", email)
	if err != nil {
		return false
	}
	defer result.Close()

	var exists bool
	result.Iterate(func(r *chai.Row) error {
		exists = true
		return nil
	})
	return exists
}

func (app *App) processPatientRegistration(email, content string) error {
	var patient Patient
	patient.Email = email

	// Extract name using regex
	nameRegex := regexp.MustCompile(`I'm ([^,\.]+)`)
	if matches := nameRegex.FindStringSubmatch(content); len(matches) > 1 {
		patient.Name = strings.TrimSpace(matches[1])
	}

	// Extract phone number
	phoneRegex := regexp.MustCompile(`\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}`)
	if phone := phoneRegex.FindString(content); phone != "" {
		patient.PhoneNumber = phone
	}

	// Extract other fields based on content
	if strings.Contains(strings.ToLower(content), "budget") {
		// Extract budget amount
		budgetRegex := regexp.MustCompile(`\$?(\d+)(?:\.?\d*)?\/hour`)
		if matches := budgetRegex.FindStringSubmatch(content); len(matches) > 1 {
			if budget, err := strconv.ParseFloat(matches[1], 64); err == nil {
				patient.Budget = budget
			}
		}
	}

	// Extract location if mentioned
	if strings.Contains(strings.ToLower(content), "located in") ||
		strings.Contains(strings.ToLower(content), "location") {
		locationRegex := regexp.MustCompile(`(?:located in|location[:\s]+)(.*?)(?:\.|$)`)
		if matches := locationRegex.FindStringSubmatch(content); len(matches) > 1 {
			patient.Location = strings.TrimSpace(matches[1])
		}
	}

	// Extract schedule requirements
	if strings.Contains(strings.ToLower(content), "need") &&
		(strings.Contains(strings.ToLower(content), "schedule") ||
			strings.Contains(strings.ToLower(content), "hours")) {
		scheduleRegex := regexp.MustCompile(`need[^.]*(?:schedule|hours)[^.]*\.`)
		if schedule := scheduleRegex.FindString(content); schedule != "" {
			patient.ScheduleRequirements = strings.TrimSpace(schedule)
		}
	}

	// Extract care needs and special requirements
	if strings.Contains(strings.ToLower(content), "need") ||
		strings.Contains(strings.ToLower(content), "require") {
		needsRegex := regexp.MustCompile(`(?:need|require)[^.]*\.`)
		if needs := needsRegex.FindString(content); needs != "" {
			if patient.CareNeeds == "" {
				patient.CareNeeds = strings.TrimSpace(needs)
			} else {
				patient.SpecialRequirements = strings.TrimSpace(needs)
			}
		}
	}

	// Only store patient if we have the required fields
	if patient.Location != "" && patient.Budget > 0 && patient.PhoneNumber != "" && patient.Name != "" {
		if err := app.StorePatient(&patient); err != nil {
			return fmt.Errorf("failed to store patient: %v", err)
		}
		return nil
	}

	return fmt.Errorf("missing required patient information")
}

func (app *App) processCaregiverRegistration(email, content string) error {
	var caregiver Caregiver
	caregiver.Email = email

	// Extract name using regex
	nameRegex := regexp.MustCompile(`I'm ([^,\.]+)`)
	if matches := nameRegex.FindStringSubmatch(content); len(matches) > 1 {
		caregiver.Name = strings.TrimSpace(matches[1])
	}

	// Extract other fields based on content
	if strings.Contains(strings.ToLower(content), "budget") {
		// Extract budget amount
		budgetRegex := regexp.MustCompile(`\$?(\d+)(?:\.?\d*)?\/hour`)
		if matches := budgetRegex.FindStringSubmatch(content); len(matches) > 1 {
			if budget, err := strconv.ParseFloat(matches[1], 64); err == nil {
				caregiver.RateExpectations = budget
			}
		}
	}

	// Extract location if mentioned
	if strings.Contains(strings.ToLower(content), "located in") ||
		strings.Contains(strings.ToLower(content), "location") {
		locationRegex := regexp.MustCompile(`(?:located in|location[:\s]+)(.*?)(?:\.|$)`)
		if matches := locationRegex.FindStringSubmatch(content); len(matches) > 1 {
			caregiver.Location = strings.TrimSpace(matches[1])
		}
	}

	// Extract schedule requirements
	if strings.Contains(strings.ToLower(content), "need") &&
		(strings.Contains(strings.ToLower(content), "schedule") ||
			strings.Contains(strings.ToLower(content), "hours")) {
		scheduleRegex := regexp.MustCompile(`need[^.]*(?:schedule|hours)[^.]*\.`)
		if schedule := scheduleRegex.FindString(content); schedule != "" {
			caregiver.Experience = strings.TrimSpace(schedule)
		}
	}

	// Extract care needs and special requirements
	if strings.Contains(strings.ToLower(content), "need") ||
		strings.Contains(strings.ToLower(content), "require") {
		needsRegex := regexp.MustCompile(`(?:need|require)[^.]*\.`)
		if needs := needsRegex.FindString(content); needs != "" {
			if caregiver.Experience == "" {
				caregiver.Experience = strings.TrimSpace(needs)
			} else {
				caregiver.Certifications = strings.TrimSpace(needs)
			}
		}
	}

	// Only store caregiver if we have the required fields
	if caregiver.Location != "" && caregiver.RateExpectations > 0 && caregiver.Name != "" {
		if err := app.StoreCaregiver(&caregiver); err != nil {
			return fmt.Errorf("failed to store caregiver: %v", err)
		}
		return nil
	}

	return fmt.Errorf("missing required caregiver information")
}
