package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/generative-ai-go/genai"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/mailjet/mailjet-apiv3-go/v4"

	"github.com/gomarkdown/markdown"
	"github.com/microcosm-cc/bluemonday"
)

const otpChars = "1234567890"

func generateJWT(email string) (string, string, error) {
	jti := uuid.NewString()

	claims := jwt.MapClaims{
		"iss": "geminui-server",
		"sub": email,
		"exp": time.Now().Add(time.Hour * 24 * 28).Unix(),
		"jti": jti,
		"iat": time.Now().Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(SECRET))
	if err != nil {
		return "", "", err
	}

	return tokenString, jti, nil
}

func parseJWT(tokenString string) (*TokenInfo, error) {
	token, err := jwt.Parse(
		tokenString,
		func(token *jwt.Token) (interface{}, error) {
			return []byte(SECRET), nil
		},
	)

	if err != nil {
		return nil, err
	}

	var tokenInfo *TokenInfo

	claims, ok := token.Claims.(jwt.MapClaims)
	if ok && token.Valid {
		email, _ := claims.GetSubject()
		jti := claims["jti"].(string)
		expiration, _ := claims.GetExpirationTime()

		var result bson.M
		users.FindOne(context.TODO(), bson.D{{Key: "email", Value: email}}).Decode(&result)

		jtis := result["jtis"]

		found := false
		for _, value := range jtis.(bson.A) {
			if value == jti {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("expired token")
		}

		if expiration.Unix() < time.Now().Unix() {
			return nil, errors.New("expired token")
		}

		tokenInfo = &TokenInfo{
			Email: email,
			jti:   jti,
			ID:    result["_id"].(primitive.ObjectID),
		}
	} else {
		return nil, errors.New("invalid token")
	}

	return tokenInfo, nil
}

func generateSecret(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func generateOTP(length int) (string, error) { // taken from https://stackoverflow.com/a/61600241
	buffer := make([]byte, length)
	_, err := rand.Read(buffer)
	if err != nil {
		return "", err
	}

	otpCharsLength := len(otpChars)
	for i := 0; i < length; i++ {
		buffer[i] = otpChars[int(buffer[i])%otpCharsLength]
	}

	return string(buffer), nil
}

func ObjectIDToString(i primitive.ObjectID) string {
	return i.Hex()
}

func ObjectIDFromHex(s string) (primitive.ObjectID, error) {
	objID, err := primitive.ObjectIDFromHex(s)
	if err != nil {
		return primitive.ObjectID{}, err
	}

	return objID, nil
}

func sendVerificationEmail(email, name, otp string) (*mailjet.ResultsV31, string, error) {
	verifDoc, err := emailVerification.InsertOne(ctx, bson.D{
		{Key: "email", Value: email},
		{Key: "code", Value: otp},
	})
	if err != nil {
		return nil, "", err
	}

	messagesInfo := []mailjet.InfoMessagesV31{
		{
			From: &mailjet.RecipientV31{
				Email: EMAIL_SENDER,
				Name:  "[noreply] GeminUI",
			},
			To: &mailjet.RecipientsV31{
				mailjet.RecipientV31{
					Email: email,
					Name:  name,
				},
			},
			Subject:  "Your verification code",
			TextPart: "Hi " + name + ",\nYour one-time login code is:\n" + otp,
		},
	}
	messages := mailjet.MessagesV31{Info: messagesInfo}
	mail, err := mailjetClient.SendMailV31(&messages)
	if err != nil {
		return nil, "", err
	}

	return mail, verifDoc.InsertedID.(primitive.ObjectID).Hex(), nil
}

func convertToGenaiContent(history []Content) []*genai.Content {
	var content []*genai.Content
	for _, v := range history {
		content = append(
			content, &genai.Content{
				Parts: []genai.Part{
					genai.Text(v.Parts[0]),
				},
				Role: v.Role,
			},
		)
	}
	return content
}

func convertToInterface(history []*genai.Content) []interface{} {
	var content []interface{}
	for _, v := range history {
		content = append(content, v)
	}
	return content
}

func markdownToHTML(text string) string {
	unsafe := markdown.ToHTML([]byte(text), nil, nil)
	html := bluemonday.UGCPolicy().SanitizeBytes(unsafe)
	return string(html)
}

func reverse[T any](list []T) []T {
	for i, j := 0, len(list)-1; i < j; {
		list[i], list[j] = list[j], list[i]
		i++
		j--
	}
	return list
}

func replace(input, from, to string) string {
	return strings.Replace(input, from, to, -1)
}
