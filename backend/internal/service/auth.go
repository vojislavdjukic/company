package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrInvalidToken       = errors.New("invalid or expired bearer token")
	ErrEmailTaken         = errors.New("email is already registered")
	ErrInvalidInput       = errors.New("email and password are required; password must be at least 8 characters")
)

// User is the public account identity returned by authentication operations.
type User struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// AuthRepository stores account credentials and roles used to issue JWTs.
type AuthRepository interface {
	CreateUser(context.Context, string, string) (int64, string, error)
	FindUserByEmail(context.Context, string) (int64, string, string, string, error)
}

// UserEmailLookup loads the current account email for a verified token's user ID.
// ctx bounds the query; userID is the JWT subject.
type UserEmailLookup interface {
	FindUserEmailByID(context.Context, int64) (string, error)
}

// AuthService handles account creation, password verification, and bearer-token validation.
type AuthService struct {
	repository AuthRepository
	signingKey []byte
}

// NewAuthService constructs an authentication service backed by repository and a JWT signing key.
// repository stores account records; signingKey must contain at least 32 bytes and remain stable across restarts.
func NewAuthService(repository AuthRepository, signingKey string) (*AuthService, error) {
	if len(signingKey) < 32 {
		return nil, errors.New("JWT_SECRET must contain at least 32 characters")
	}
	return &AuthService{repository: repository, signingKey: []byte(signingKey)}, nil
}

// Register validates email and password, stores a bcrypt password hash, and returns a signed JWT.
// ctx bounds storage operations; email and password are the credentials supplied by the user.
func (s *AuthService) Register(ctx context.Context, email, password string) (User, string, error) {
	email = normalizeEmail(email)
	if !validEmail(email) || len(password) < 8 || len(password) > 72 {
		return User{}, "", ErrInvalidInput
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, "", fmt.Errorf("hash password: %w", err)
	}
	userID, storedEmail, err := s.repository.CreateUser(ctx, email, string(passwordHash))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") || strings.Contains(strings.ToLower(err.Error()), "unique") {
			return User{}, "", ErrEmailTaken
		}
		return User{}, "", err
	}
	user := User{ID: userID, Email: storedEmail, Role: "user"}
	token, err := s.createToken(user)
	return user, token, err
}

// Login verifies credentials and returns a signed JWT containing the account role.
// ctx bounds storage operations; email and password are the credentials to verify.
func (s *AuthService) Login(ctx context.Context, email, password string) (User, string, error) {
	userID, storedEmail, role, passwordHash, err := s.repository.FindUserByEmail(ctx, normalizeEmail(email))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrInvalidCredentials
	}
	if err != nil {
		return User{}, "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) != nil {
		return User{}, "", ErrInvalidCredentials
	}
	if !validRole(role) {
		return User{}, "", errors.New("account has an unsupported role")
	}
	user := User{ID: userID, Email: storedEmail, Role: role}
	token, err := s.createToken(user)
	return user, token, err
}

// Authenticate verifies a signed, unexpired JWT and returns its account identity.
// ctx bounds the optional email lookup; token is the raw bearer JWT.
func (s *AuthService) Authenticate(ctx context.Context, token string) (User, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return User{}, ErrInvalidToken
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(headerBytes) != `{"alg":"HS256","typ":"JWT"}` {
		return User{}, ErrInvalidToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return User{}, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, s.signingKey)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return User{}, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return User{}, ErrInvalidToken
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Issuer != "company-backend" || claims.ExpiresAt <= time.Now().Unix() || claims.IssuedAt > time.Now().Add(time.Minute).Unix() || !validRole(claims.Role) {
		return User{}, ErrInvalidToken
	}
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || userID <= 0 {
		return User{}, ErrInvalidToken
	}
	user := User{ID: userID, Role: claims.Role}
	if emailLookup, ok := s.repository.(UserEmailLookup); ok {
		email, err := emailLookup.FindUserEmailByID(ctx, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrInvalidToken
		}
		if err != nil {
			return User{}, err
		}
		user.Email = email
	}
	return user, nil
}

type jwtClaims struct {
	Subject   string `json:"sub"`
	Role      string `json:"role"`
	Issuer    string `json:"iss"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// createToken signs a seven-day HS256 JWT containing the user ID and role.
// user supplies the ID and role encoded in the token claims.
func (s *AuthService) createToken(user User) (string, error) {
	now := time.Now()
	claims := jwtClaims{
		Subject: strconv.FormatInt(user.ID, 10), Role: user.Role,
		Issuer: "company-backend", IssuedAt: now.Unix(), ExpiresAt: now.Add(7 * 24 * time.Hour).Unix(),
	}
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("encode JWT header: %w", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode JWT claims: %w", err)
	}
	message := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.signingKey)
	_, _ = mac.Write([]byte(message))
	return message + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// validRole accepts the role values currently issued by the application.
// role is the account role read from persistence or an incoming JWT claim.
func validRole(role string) bool { return role == "user" || role == "admin" }

// normalizeEmail trims surrounding whitespace and folds the address to lowercase.
// email is the submitted account address.
func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// validEmail performs basic structural validation before storing an address.
// email is the normalized address to validate.
func validEmail(email string) bool {
	if len(email) > 254 || strings.Count(email, "@") != 1 {
		return false
	}
	parts := strings.SplitN(email, "@", 2)
	return parts[0] != "" && strings.Contains(parts[1], ".") && !strings.HasPrefix(parts[1], ".") && !strings.HasSuffix(parts[1], ".")
}
