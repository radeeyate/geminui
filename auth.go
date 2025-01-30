package main

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func handleJoinPage(c *fiber.Ctx) error {
	token := c.Cookies("token", "")

	if token == "" {
		return c.Render("join", fiber.Map{})
	} else {
		_, err := parseJWT(token)

		if err != nil {
			c.ClearCookie("token")
			return c.Render("join", fiber.Map{})
		}

		return c.Redirect("/", 302)
	}
}

func handleJoin(c *fiber.Ctx) error {
	email := c.FormValue("email")
	studentID := c.FormValue("id")
	name := c.FormValue("name")

	if email == "" || studentID == "" || name == "" {
		return c.Render(
			"join",
			fiber.Map{"Error": "An email, name, and student ID must be provided"},
		)
	}

	result := users.FindOne(ctx, bson.M{"email": email})
	if err := result.Err(); err == nil { //Check if a user with this email already exists
		return c.Render(
			"join",
			fiber.Map{"Error": "A user with this student ID already exists"},
		)
	}

	ret, err := verifier.Verify(email)
	if err != nil || !ret.Syntax.Valid {
		return c.Render(
			"join",
			fiber.Map{"Error": "An error occured or email address syntax is invalid"},
		)
	}

	if ret.Syntax.Domain != EMAIL_DOMAIN {
		return c.Render(
			"join",
			fiber.Map{"Error": "Invalid email domain"},
		)
	}

	otpCode, err := generateOTP(6)
	if err != nil {
		return c.Render(
			"join",
			fiber.Map{"Error": "An unknown error occured: " + err.Error()},
		)
	}

	_, err = users.InsertOne(ctx, bson.D{
		{Key: "studentID", Value: studentID},
		{Key: "email", Value: email},
		{Key: "name", Value: name},
		{Key: "jtis", Value: []string{}},
		{Key: "emailVerified", Value: false},
	})
	if err != nil {
		return c.Render(
			"join",
			fiber.Map{"Error": "An unknown error occured: " + err.Error()},
		)
	}

	_, verificationID, err := sendVerificationEmail(email, name, otpCode)
	if err != nil {
		return c.Render(
			"join",
			fiber.Map{"Error": "An unknown error occured: " + err.Error()},
		)
	}

	return c.Redirect(
		"/verify/" + verificationID,
	)
}

func handleLoginPage(c *fiber.Ctx) error {
	token := c.Cookies("token", "")

	if token == "" {
		return c.Render("login", fiber.Map{})
	} else {
		_, err := parseJWT(token)

		if err != nil {
			c.ClearCookie("token")
			return c.Render("login", fiber.Map{})
		}

		return c.Redirect("/", 302)
	}
}

func handleLogin(c *fiber.Ctx) error {
	email := c.FormValue("email", "")

	if email == "" {
		return c.Render(
			"join",
			fiber.Map{"Error": "An email must be provided"},
		)
	}

	result := users.FindOne(
		ctx, bson.D{{Key: "email", Value: email}},
	)
	if result.Err() == mongo.ErrNoDocuments { // user doesn't exist
		return c.Render(
			"login",
			fiber.Map{"Error": "Incorrect email address"},
		)
	}

	var user User
	err := users.FindOne(context.TODO(), bson.M{"email": email}).Decode(&user)
	if err != nil {
		return c.Render(
			"login",
			fiber.Map{
				"Error": "An error occured while trying to login to your account.",
			},
		)
	}

	otpCode, err := generateOTP(6)
	if err != nil {
		return c.Render(
			"join",
			fiber.Map{"Error": "An unknown error occured: " + err.Error()},
		)
	}

	_, verificationID, err := sendVerificationEmail(email, user.Name, otpCode)
	if err != nil {
		return c.Render(
			"join",
			fiber.Map{"Error": "An unknown error occured: " + err.Error()},
		)
	}

	return c.Redirect(
		"/verify/" + verificationID,
	)
}

func handleVerificationPage(c *fiber.Ctx) error {
	id := c.Params("id")

	objID, err := ObjectIDFromHex(id)
	if err != nil { // TODO: implement 404s and stuff
		return c.SendString("not found")
	}

	result := emailVerification.FindOne(ctx, bson.M{"_id": objID})
	if result.Err() == mongo.ErrNoDocuments { // verification doesn't exist
		return c.SendString("not found")
	}

	return c.Render("verify-email", fiber.Map{"ID": id})
}

func handleVerification(c *fiber.Ctx) error {
	id := c.Params("id")
	otp := c.FormValue("otp")

	objID, err := ObjectIDFromHex(id)
	if err != nil {
		return c.SendStatus(fiber.StatusNotFound)
	}

	var verification Verification
	err = emailVerification.FindOne(ctx, bson.M{"_id": objID}).Decode(&verification)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return c.SendStatus(fiber.StatusNotFound)
		}
		return c.Status(fiber.StatusInternalServerError).SendString("Database error")
	}

	if otp == verification.Code {
		_, err := emailVerification.DeleteOne(ctx, bson.D{{Key: "_id", Value: objID}})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("Database error")
		}

		_, err = users.UpdateOne(
			ctx,
			bson.M{"email": verification.Code},
			bson.M{"$set": bson.M{"emailVerified": true}},
		)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("Database error")
		}

		token, jti, err := generateJWT(verification.Email)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("an unknown error")
		}

		c.Cookie(&fiber.Cookie{
			Name:    "token",
			Value:   token,
			Expires: time.Now().Add(time.Hour * 24 * 28),
		})

		_, err = users.UpdateOne(
			ctx,
			bson.M{"email": verification.Email},
			bson.D{
				{Key: "$push", Value: bson.D{{Key: "jtis", Value: jti}}},
				{Key: "$set", Value: bson.D{{Key: "emailVerified", Value: true}}},
			},
			options.Update().SetUpsert(true),
		)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("an unknown error")
		}

		return c.Redirect("/", fiber.StatusFound)
	} else {
		return c.Render("verify-email", fiber.Map{"ID": id, "Error": "Invalid OTP Code"})
	}
}
