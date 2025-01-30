package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"html/template"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/template/html/v2"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/mailjet/mailjet-apiv3-go/v4"

	emailverifier "github.com/AfterShip/email-verifier"
)

var GEMINI_API_KEY string
var CONNECTION_STRING string
var SECRET string
var EMAIL_DOMAIN string
var MAILJET_PRIVATE string
var MAILJET_PUBLIC string
var EMAIL_SENDER string
var TIMEZONE string
var ctx = context.TODO()
var users *mongo.Collection
var emailVerification *mongo.Collection
var chats *mongo.Collection
var uploads *mongo.Collection
var database *mongo.Database
var mailjetClient *mailjet.Client

type TokenInfo struct {
	Email string
	jti   string
	ID    primitive.ObjectID `bson:"_id"`
}

type User struct {
	ID            primitive.ObjectID `bson:"_id"`
	StudentID     string
	Email         string
	Name          string
	JTI           []string
	EmailVerified bool
}

type Verification struct {
	ID    primitive.ObjectID `bson:"_id"`
	Email string
	Code  string
}

type Chat struct {
	ID      primitive.ObjectID `bson:"_id"`
	User    primitive.ObjectID `bson:"user"`
	Title   string             `bson:"title"`
	History []interface{}      `bson:"history"`
	Model   string             `bson:"model"`
}

type ContentChat struct {
	ID      primitive.ObjectID `bson:"_id"`
	User    primitive.ObjectID `bson:"user"`
	History []Content          `bson:"history"`
	Model   string             `bson:"model"`
}

type File struct {
	ID   primitive.ObjectID `bson:"_id"`
	User primitive.ObjectID `bson:"user"`
	Name string             `bson:"name"`
	Path string             `bson:"path"`
}

type Content struct {
	Parts []string
	Role  string
}

var (
	verifier = emailverifier.NewVerifier()
)

func main() {
	godotenv.Load()
	GEMINI_API_KEY = os.Getenv("GEMINI_API_KEY")
	CONNECTION_STRING = os.Getenv("CONNECTION_STRING")
	SECRET = os.Getenv("SECRET")
	EMAIL_DOMAIN = os.Getenv("EMAIL_DOMAIN")
	MAILJET_PRIVATE = os.Getenv("MAILJET_PRIVATE")
	MAILJET_PUBLIC = os.Getenv("MAILJET_PUBLIC")
	EMAIL_SENDER = os.Getenv("EMAIL_SENDER")
	TIMEZONE = os.Getenv("TIMEZONE")

	mailjetClient = mailjet.NewMailjetClient(MAILJET_PUBLIC, MAILJET_PRIVATE)

	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(GEMINI_API_KEY))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	var models = make(map[string]*genai.GenerativeModel)

	models["gemini-1.5-flash-8b"] = client.GenerativeModel("gemini-1.5-flash-8b")
	models["gemini-1.5-flash"] = client.GenerativeModel("gemini-1.5-flash")
	models["gemini-2.0-flash-exp"] = client.GenerativeModel("gemini-2.0-flash-exp")
	models["summarize"] = client.GenerativeModel("gemini-1.5-flash-8b")

	models["summarize"].SystemInstruction = &genai.Content{
		Parts: []genai.Part{
			genai.Text(
				"You are a title generator for conversations between humans. Create concise, engaging, and relevant titles based on the provided conversation content. Do not provide titles in Markdown. Do not return multiple responses. Do not provide anything related to that it is a conversation. Do not answer or reply to the initial statement.",
			),
		},
	}

	engine := html.New("./templates", ".html")
	engine.AddFunc("idtostring", func(id primitive.ObjectID) string { return id.Hex() })
	engine.AddFunc("mdtohtml", markdownToHTML)
	engine.AddFunc("htmlSafe", func(html string) template.HTML {
		return template.HTML(html)
	})
	engine.AddFunc("replace", replace)
	engine.Reload(true)
	app := fiber.New(fiber.Config{Views: engine})
	app.Static("/static", "./static")
	app.Use(logger.New(logger.Config{
		Format:     "${time} - ${status} - ${ip} ${method} ${path}\n",
		TimeFormat: "15:04:05 on 01/02/2006",
		TimeZone:   "America/Denver",
	}))

	app.Get("/", func(c *fiber.Ctx) error {
		token := c.Cookies("token", "")

		if token == "" {
			return c.Redirect("/login", 302)
		} else {
			parsedToken, err := parseJWT(token)

			if err != nil {
				c.ClearCookie("token")
				return c.Redirect("/login", 302)
			}

			var user User
			err = users.FindOne(context.TODO(), bson.M{"_id": parsedToken.ID}).Decode(&user)
			if err != nil {
				return c.Status(fiber.StatusNotFound).SendString("error: an unknown error occured")
			}

			cursor, err := chats.Find(ctx, bson.M{"user": user.ID})
			if err != nil {
				return fiber.ErrNotFound
			}

			var chatList []Chat
			if err = cursor.All(ctx, &chatList); err != nil {
				// idk
			}

			chatList = reverse(chatList)

			return c.Render("index", fiber.Map{"Chats": chatList, "User": user})
		}
	})

	app.Get("/login", handleLoginPage)
	app.Get("/join", handleJoinPage)
	app.Post("/login", handleLogin)
	app.Post("/join", handleJoin)
	app.Get("/verify/:id", handleVerificationPage)
	app.Post("/verify/:id", handleVerification)

	app.Post("/api/ask", func(c *fiber.Ctx) error {
		token := c.Cookies("token", "")
		question := c.FormValue("question")
		chosenModel := c.FormValue("model", "gemini-1.5-flash")
		id := c.Query("chat", "new")

		var parsedToken *TokenInfo

		if token == "" {
			return c.Status(fiber.StatusUnauthorized).SendString("error: unauthorized")
		} else {
			parsedToken, err = parseJWT(token)

			if err != nil {
				c.ClearCookie(token)
				return c.Status(fiber.StatusUnauthorized).SendString("error: unauthorized")
			}
		}

		var user User
		err := users.FindOne(context.TODO(), bson.M{"email": parsedToken.Email}).Decode(&user)
		if err != nil {
			return c.Status(fiber.StatusNotFound).SendString("error: an unknown error occured")
		}

		var chat ContentChat

		if id != "new" {
			objID, err := ObjectIDFromHex(id)
			if err != nil {
				return c.Status(fiber.StatusNotFound).SendString("error: an unknown error occured")
			}

			result := chats.FindOne(ctx, bson.D{{Key: "_id", Value: objID}})
			if result.Err() == mongo.ErrNoDocuments {
				return c.Status(fiber.StatusNotFound).SendString("error: not found")
			}

			err = chats.FindOne(ctx, bson.D{{Key: "_id", Value: objID}}).Decode(&chat)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).
					SendString("error: an unknown error occured")
			}

			if chat.User != user.ID {
				return c.Status(fiber.StatusForbidden).SendString("error: forbidden")
			}

			chosenModel = chat.Model
		}

		if !slices.Contains(
			[]string{"gemini-1.5-flash", "gemini-1.5-flash-8b", "gemini-2.0-flash-exp"},
			chosenModel,
		) {
			return c.Status(fiber.StatusForbidden).SendString("error: invalid model")
		}

		model := models[chosenModel]

		loc, _ := time.LoadLocation(TIMEZONE)
		now := time.Now().In(loc)

		model.SystemInstruction = &genai.Content{
			Parts: []genai.Part{
				genai.Text(
					"The current time is " + now.Format(time.Kitchen) + " on " + now.Format(time.DateOnly) + ".",
				),
			},
		}

		cs := model.StartChat()
		var title string

		if id != "new" {
			cs.History = convertToGenaiContent(chat.History)
		} else {
			response, err := models["summarize"].GenerateContent(ctx, genai.Text("Write a max 5 word title for an AI chat with this as the first question: "+question))
			if err != nil {
				log.Fatal(err)
			}

			title = strings.TrimSpace(fmt.Sprintf("%v", response.Candidates[0].Content.Parts[0]))
		}

		answer := cs.SendMessageStream(ctx, genai.Text(question))

		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")

		c.Response().SetBodyStreamWriter(func(w *bufio.Writer) {
			for {
				resp, err := answer.Next()
				if err == iterator.Done {
					if id == "new" {
						_, err := chats.InsertOne(ctx, Chat{
							ID:      primitive.NewObjectID(),
							User:    user.ID,
							Title:   title,
							History: convertToInterface(cs.History),
							Model:   chosenModel,
						})
						if err != nil {
							log.Fatal(err)
						}
					} else {
						_, err := chats.UpdateOne(
							ctx,
							bson.M{"_id": chat.ID},
							bson.M{"$set": bson.M{"history": cs.History}},
						)
						if err != nil {
							log.Fatal(err)
						}
					}

					return
				}
				if err != nil {
					w.Write([]byte("Error: an error occured: " + err.Error()))
					return
				}

				data := []byte(fmt.Sprintf("%s", resp.Candidates[0].Content.Parts[0]))
				if _, err := w.Write(data); err != nil {
					log.Printf("Error writing to stream: %v", err)
					return
				}
				err = w.Flush()
				if err != nil {
					log.Printf("Error flushing stream: %v", err)
					return
				}
			}
		})

		return nil
	})

	app.Get("/api/newest", func(c *fiber.Ctx) error {
		token := c.Cookies("token", "")

		var parsedToken *TokenInfo

		if token == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		} else {
			parsedToken, err = parseJWT(token)

			if err != nil {
				c.ClearCookie(token)
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
			}
		}

		var user User
		if err = users.FindOne(ctx, bson.M{"email": parsedToken.Email}).Decode(&user); err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}

		var chat Chat
		if err = chats.FindOne(ctx, bson.M{"user": user.ID}, options.FindOne().SetSort(bson.M{"$natural": -1})).Decode(&chat); err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "none found"})
		}

		return c.JSON(chat)
	})

	app.Post("/api/upload", func(c *fiber.Ctx) error {
		file, err := c.FormFile("file")
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "no file provided"})
		}

		token := c.Cookies("token", "")
		var parsedToken *TokenInfo

		if token == "" {
			return c.Status(fiber.StatusUnauthorized).SendString("error: unauthorized")
		} else {
			parsedToken, err = parseJWT(token)

			if err != nil {
				c.ClearCookie(token)
				return c.Status(fiber.StatusUnauthorized).SendString("error: unauthorized")
			}
		}

		var user User
		err = users.FindOne(context.TODO(), bson.M{"email": parsedToken.Email}).Decode(&user)
		if err != nil {
			return c.Status(fiber.StatusNotFound).SendString("error: an unknown error occured")
		}

		var filename string

		f, err := file.Open()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "an error occured while trying to save the file"})
		}

		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "an error occured while trying to save the file"})
		}

		filename = string(h.Sum(nil))

		c.SaveFile(file, "./uploads/" + filename)

		_, err = uploads.InsertOne(ctx, File{
			ID:      primitive.NewObjectID(),
			User:    user.ID,
			Name:   file.Filename,
			Path: "./uploads/" + filename,
		})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("error: an unknown error occured")
		}

		return c.JSON(fiber.Map{"ok": "file uploaded successfully"})

	})

	app.Get("/chat/:id", func(c *fiber.Ctx) error {
		token := c.Cookies("token", "")
		id := c.Params("id", "new")

		var parsedToken *TokenInfo

		if token == "" {
			return c.Status(fiber.StatusUnauthorized).SendString("error: unauthorized")
		} else {
			parsedToken, err = parseJWT(token)

			if err != nil { // TODO: better error handling
				c.ClearCookie(token)
				return c.Status(fiber.StatusUnauthorized).SendString("error: unauthorized: " + err.Error())
			}
		}

		var user User
		if err = users.FindOne(ctx, bson.M{"email": parsedToken.Email}).Decode(&user); err != nil {
			return c.Status(fiber.StatusUnauthorized).SendString("error: unauthorized")
		}

		objID, err := ObjectIDFromHex(id)
		if err != nil {
			// do something
		}

		var chat ContentChat
		if err = chats.FindOne(ctx, bson.M{"_id": objID}).Decode(&chat); err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "none found: " + err.Error()})
		}

		if chat.User != user.ID {
			return c.Redirect("/", 302)
		}

		cursor, err := chats.Find(ctx, bson.M{"user": user.ID})
		if err != nil {
			return fiber.ErrNotFound
		}

		var chatList []Chat
		if err = cursor.All(ctx, &chatList); err != nil {
			// idk
		}

		chatList = reverse(chatList)

		return c.Render("chat", fiber.Map{"Chat": chat, "Chats": chatList})
	})

	app.Get("/favicon.ico", func(c *fiber.Ctx) error {
		return c.SendFile("./static/favicon.ico")
	})

	app.Delete("/api/delete/:id", func(c *fiber.Ctx) error {
		token := c.Cookies("token", "")
		id := c.Params("id", "")
		chatId, err := ObjectIDFromHex(id)

		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad id"})
		}

		var parsedToken *TokenInfo

		if token == "" {
			fmt.Println("rahh")
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		} else {
			parsedToken, err = parseJWT(token)

			if err != nil {
				c.ClearCookie(token)
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
			}
		}

		var user User
		if err = users.FindOne(ctx, bson.M{"email": parsedToken.Email}).Decode(&user); err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}

		var chat Chat
		if err = chats.FindOne(ctx, bson.M{"_id": chatId}).Decode(&chat); err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "chat not found"})
		}

		if _, err = chats.DeleteOne(ctx, bson.M{"_id": chatId}); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "unable to delete chat"})
		}

		return c.JSON(fiber.Map{"ok": "chat deleted successfully"})
	})

	connect()
	log.Fatal(app.Listen(":3000"))
}

func connect() {
	clientOptions := options.Client().
		ApplyURI(CONNECTION_STRING)
	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		log.Fatal(err)
	}

	err = client.Ping(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}

	database = client.Database("geminui")

	users = database.Collection("users")
	chats = database.Collection("chats")
	emailVerification = database.Collection("email-verification")
	uploads = database.Collection("uploads")

	fmt.Println("Connected to MongoDB!")
}
