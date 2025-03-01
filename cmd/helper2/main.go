package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
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
        .user-email {
            text-align: right;
            color: #666;
            margin-bottom: 10px;
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
        <div class="user-email">Logged in as: {{.UserEmail}}</div>
        <div id="messages">
            {{range .Messages}}
            <div class="message {{.Role}}">
                <strong>{{.Role}}:</strong> {{.Content | html}}
            </div>
            {{end}}
        </div>
        <form method="POST" action="/chat" class="message-form">
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
			care_needs TEXT,
			location TEXT,
			schedule_requirements TEXT,
			budget REAL,
			special_requirements TEXT,
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
			SET experience = ?,
				location = ?,
				availability = ?,
				specializations = ?,
				rate_expectations = ?,
				certifications = ?
			WHERE email = ?
		`, c.Experience, c.Location, c.Availability,
			c.Specializations, c.RateExpectations, c.Certifications,
			c.Email)
	}

	// Insert new caregiver
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
			"name":        "store_caregiver",
			"description": "Store a new caregiver's information in the system",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"email": map[string]interface{}{
						"type":        "string",
						"description": "Caregiver's email address",
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
				"required": []string{"email", "location", "rate_expectations"},
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
				"required": []string{"email", "care_needs", "location"},
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
		if len(chatResp.Choices) > 0 {
			choice := chatResp.Choices[0].Message
			if choice.FunctionCall != nil {
				args, err := choice.FunctionCall.GetArguments()
				if err != nil {
					log.Printf("Error parsing function arguments: %v", err)
					http.Error(w, "Failed to process function call", http.StatusInternalServerError)
					return
				}

				var response string
				switch choice.FunctionCall.Name {
				case "list_patients":
					patients, err := chatRoom.ListPatients()
					if err != nil {
						response = fmt.Sprintf("Error listing patients: %v", err)
					} else {
						response = formatPatientList(patients)
					}

				case "list_caregivers":
					caregivers, err := chatRoom.ListCaregivers()
					if err != nil {
						response = fmt.Sprintf("Error listing caregivers: %v", err)
					} else {
						response = formatCaregiverList(caregivers)
					}

				case "find_matching_caregivers":
					patientEmail := getStringArg(args, "patient_email", userEmail) // Use current user's email if not specified
					caregivers, err := chatRoom.FindMatchingCaregivers(patientEmail)
					if err != nil {
						response = fmt.Sprintf("Error finding matches: %v", err)
					} else {
						response = formatCaregiverList(caregivers)
					}

				case "store_caregiver":
					caregiver := &Caregiver{
						Email:            getStringArg(args, "email", ""),
						Experience:       getStringArg(args, "experience", ""),
						Location:         getStringArg(args, "location", ""),
						Availability:     getStringArg(args, "availability", ""),
						Specializations:  getStringArg(args, "specializations", ""),
						RateExpectations: getFloatArg(args, "rate_expectations", 0),
						Certifications:   getStringArg(args, "certifications", ""),
					}
					if err := chatRoom.StoreCaregiver(caregiver); err != nil {
						log.Printf("Error storing caregiver %s: %v", caregiver.Email, err)
						response = fmt.Sprintf("Error storing caregiver: %v", err)
					}

				case "store_patient":
					patient := &Patient{
						Email:                getStringArg(args, "email", ""),
						CareNeeds:            getStringArg(args, "care_needs", ""),
						Location:             getStringArg(args, "location", ""),
						ScheduleRequirements: getStringArg(args, "schedule_requirements", ""),
						Budget:               getFloatArg(args, "budget", 0),
						SpecialRequirements:  getStringArg(args, "special_requirements", ""),
					}
					if err := chatRoom.StorePatient(patient); err != nil {
						log.Printf("Error storing patient %s: %v", patient.Email, err)
						response = fmt.Sprintf("Error storing patient: %v", err)
					}

				default:
					response = "Function call not recognized"
				}

				if err := chatRoom.AddMessageWithRecipient(userEmail, "assistant", response, "admin"); err != nil {
					log.Printf("Error adding function response: %v", err)
				}
			} else if choice.Content != "" {
				if err := chatRoom.AddMessageWithRecipient(userEmail, "assistant", choice.Content, "admin"); err != nil {
					log.Printf("Error adding assistant response: %v", err)
				}
			}
		}

		http.Redirect(w, r, fmt.Sprintf("/?email=%s", url.QueryEscape(userEmail)), http.StatusSeeOther)
		return
	}

	// Handle GET request
	log.Printf("Getting messages for email: %s", userEmail)
	messages := chatRoom.GetUserMessages(userEmail)

	data := TemplateData{
		Messages:  messages,
		UserEmail: userEmail,
	}

	tmpl, err := template.New("chat").Parse(htmlTemplate)
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
		if err := r.Scan(&p.Email, &p.CareNeeds, &p.Location, &p.ScheduleRequirements,
			&p.Budget, &p.SpecialRequirements, &p.CreatedAt); err != nil {
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
		if err := r.Scan(&patient.Email, &patient.CareNeeds, &patient.Location,
			&patient.ScheduleRequirements, &patient.Budget, &patient.SpecialRequirements,
			&patient.CreatedAt); err != nil {
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

	// Now find matching caregivers
	// For now, match based on location and budget
	result, err = app.db.Query(`
		SELECT * FROM caregivers 
		WHERE location LIKE ? 
		AND rate_expectations <= ?
	`, "%"+patient.Location+"%", patient.Budget)
	if err != nil {
		return nil, fmt.Errorf("failed to query matching caregivers: %v", err)
	}
	defer result.Close()

	var caregivers []Caregiver
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
		return nil, fmt.Errorf("failed to iterate matching caregivers: %v", err)
	}

	return caregivers, nil
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

// Add handler for dynamic queries in handleChat
func handleDynamicQuery(args map[string]interface{}) (string, error) {
	// Parse the dynamic query parameters
	query := DynamicQuery{
		Table: args["table"].(string),
	}

	if fields, ok := args["fields"].([]interface{}); ok {
		for _, f := range fields {
			query.Fields = append(query.Fields, f.(string))
		}
	}

	if filters, ok := args["filters"].([]interface{}); ok {
		for _, f := range filters {
			filter := f.(map[string]interface{})
			query.Filters = append(query.Filters, QueryFilter{
				Field:    filter["field"].(string),
				Operator: filter["operator"].(string),
				Value:    filter["value"],
			})
		}
	}

	if orderBy, ok := args["order_by"].(string); ok {
		query.OrderBy = orderBy
	}

	if limit, ok := args["limit"].(float64); ok {
		query.Limit = int(limit)
	}

	// Execute the query
	results, err := chatRoom.ExecuteDynamicQuery(query)
	if err != nil {
		return "", err
	}

	// Format results as readable text
	var response strings.Builder
	response.WriteString(fmt.Sprintf("Query results from %s:\n\n", query.Table))

	for _, row := range results {
		for field, value := range row {
			response.WriteString(fmt.Sprintf("%s: %v\n", field, value))
		}
		response.WriteString("\n")
	}

	return response.String(), nil
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

// Add this to ensure the chat_history table exists
func (app *App) createSchema() error {
	err := app.db.Exec(`
		CREATE TABLE IF NOT EXISTS chat_history (
			email TEXT,
			role TEXT,
			content TEXT,
			recipient TEXT,
			created_at TIMESTAMP,
			PRIMARY KEY (email, created_at)
		);
		CREATE INDEX IF NOT EXISTS idx_chat_history_email ON chat_history(email);
	`)
	if err != nil {
		return fmt.Errorf("failed to create chat_history table: %v", err)
	}

	// ... rest of schema creation ...
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

// Add these functions to help with testing

func processTestData(app *App, filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open test data file: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	// Process each line as if it were a chat message
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		// Split line into email and message
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			log.Printf("Skipping invalid line format: %s", line)
			continue
		}

		email, message := parts[0], parts[1]

		// Add the message to the chat history
		if err := app.AddMessageWithRecipient(email, "user", message, "admin"); err != nil {
			log.Printf("Error adding test message for %s: %v", email, err)
			continue
		}

		// Get chat history for OpenAI
		messages := []Message{
			{Role: "system", Content: systemPrompt},
		}
		messages = append(messages, app.GetUserMessages(email)...)

		// Call OpenAI
		chatReq := ChatRequest{
			Model:    "gpt-3.5-turbo",
			Messages: messages,
		}

		log.Printf("Sending request to OpenAI for %s: %s", email, message)
		chatResp, err := callOpenAI(chatReq)
		if err != nil {
			log.Printf("Error calling OpenAI for %s: %v", email, err)
			continue
		}

		// Process OpenAI response
		if len(chatResp.Choices) > 0 {
			choice := chatResp.Choices[0].Message
			log.Printf("Response from OpenAI for %s: %+v", email, choice)

			if choice.FunctionCall != nil {
				// Handle function calls
				args, err := choice.FunctionCall.GetArguments()
				if err != nil {
					log.Printf("Error parsing function arguments: %v", err)
					continue
				}

				switch choice.FunctionCall.Name {
				case "store_caregiver":
					caregiver := &Caregiver{
						Email:            getStringArg(args, "email", ""),
						Experience:       getStringArg(args, "experience", ""),
						Location:         getStringArg(args, "location", ""),
						Availability:     getStringArg(args, "availability", ""),
						Specializations:  getStringArg(args, "specializations", ""),
						RateExpectations: getFloatArg(args, "rate_expectations", 0),
						Certifications:   getStringArg(args, "certifications", ""),
					}
					if err := app.StoreCaregiver(caregiver); err != nil {
						log.Printf("Error storing caregiver %s: %v", caregiver.Email, err)
					} else {
						log.Printf("Successfully stored caregiver: %s", caregiver.Email)
					}

				case "store_patient":
					patient := &Patient{
						Email:                getStringArg(args, "email", ""),
						CareNeeds:            getStringArg(args, "care_needs", ""),
						Location:             getStringArg(args, "location", ""),
						ScheduleRequirements: getStringArg(args, "schedule_requirements", ""),
						Budget:               getFloatArg(args, "budget", 0),
						SpecialRequirements:  getStringArg(args, "special_requirements", ""),
					}
					if err := app.StorePatient(patient); err != nil {
						log.Printf("Error storing patient %s: %v", patient.Email, err)
					} else {
						log.Printf("Successfully stored patient: %s", patient.Email)
					}
				}
			}

			// Store the assistant's response
			if choice.Content != "" {
				log.Printf("Assistant response to %s: %s", email, choice.Content)
				if err := app.AddMessageWithRecipient(email, "assistant", choice.Content, "admin"); err != nil {
					log.Printf("Error adding assistant response for %s: %v", email, err)
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading test data: %v", err)
	}

	return nil
}

// Add these formatting functions
func formatPatientList(patients []Patient) string {
	var sb strings.Builder
	sb.WriteString("List of Patients:\n\n")

	for _, p := range patients {
		sb.WriteString(fmt.Sprintf("Email: %s\n", p.Email))
		sb.WriteString(fmt.Sprintf("Location: %s\n", p.Location))
		sb.WriteString(fmt.Sprintf("Care Needs: %s\n", p.CareNeeds))
		sb.WriteString(fmt.Sprintf("Schedule Requirements: %s\n", p.ScheduleRequirements))
		sb.WriteString(fmt.Sprintf("Budget: $%.2f/hour\n", p.Budget))
		sb.WriteString(fmt.Sprintf("Special Requirements: %s\n", p.SpecialRequirements))
		sb.WriteString("-------------------\n")
	}

	return sb.String()
}

func formatCaregiverList(caregivers []Caregiver) string {
	var sb strings.Builder
	sb.WriteString("Here are the matching caregivers:\n\n")

	for _, c := range caregivers {
		// Create a link for each caregiver
		sb.WriteString(fmt.Sprintf("• <a href='/profile?email=%s'>%s</a>\n",
			url.QueryEscape(c.Email), c.Email))
		sb.WriteString(fmt.Sprintf("  Location: %s\n", c.Location))
		sb.WriteString(fmt.Sprintf("  Rate: $%.2f/hour\n", c.RateExpectations))
		sb.WriteString(fmt.Sprintf("  Availability: %s\n\n", c.Availability))
	}

	return sb.String()
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

	// Process test data if the file exists
	if _, err := os.Stat("testdata.txt"); err == nil {
		log.Println("Processing test data...")
		if err := processTestData(chatRoom, "testdata.txt"); err != nil {
			log.Printf("Error processing test data: %v", err)
		}
		// Print debug information after processing
		chatRoom.DebugPrintAllMessages()
	}

	port := ":8080"
	fmt.Printf("Server starting on http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
