<?php

use App\Models\User;
use App\Services\AuthService;
use Illuminate\Support\Facades\Hash;

/**
 * Sign in a factory user (their password is "password") without going through HTTP.
 *
 * @return array{access_token: string, refresh_token: string}
 */
function tokensFor(User $user): array
{
    return app(AuthService::class)->login($user->email, 'password');
}

describe('register', function () {
    test('creates the user and returns a token pair', function () {
        $response = $this->postJson('/api/v1/auth/register', [
            'name' => 'Ada Lovelace',
            'email' => 'ada@example.com',
            'password' => 'secret-password',
        ]);

        $response->assertCreated()
            ->assertJsonStructure(['access_token', 'refresh_token', 'token_type', 'expires_in'])
            ->assertJson(['token_type' => 'Bearer', 'expires_in' => 900]);
        $user = User::where('email', 'ada@example.com')->sole();
        expect(Hash::check('secret-password', $user->password))->toBeTrue();
        expect($user->refreshTokens)->toHaveCount(1);
    });

    test('returns 422 when the email is already registered', function () {
        User::factory()->create(['email' => 'ada@example.com']);

        $response = $this->postJson('/api/v1/auth/register', [
            'name' => 'Ada Lovelace',
            'email' => 'ada@example.com',
            'password' => 'secret-password',
        ]);

        $response->assertUnprocessable()
            ->assertJsonValidationErrors(['email' => 'The email has already been taken.']);
        $this->assertDatabaseCount('users', 1);
    });

    test('returns 422 when required fields are missing', function () {
        $this->postJson('/api/v1/auth/register')
            ->assertUnprocessable()
            ->assertJsonValidationErrors([
                'name' => 'The name field is required.',
                'email' => 'The email field is required.',
                'password' => 'The password field is required.',
            ]);
    });

    test('returns 422 when the password is shorter than 8 characters', function () {
        $this->postJson('/api/v1/auth/register', [
            'name' => 'Ada Lovelace',
            'email' => 'ada@example.com',
            'password' => 'short',
        ])
            ->assertUnprocessable()
            ->assertJsonValidationErrors(['password' => 'The password field must be at least 8 characters.']);
    });
});

describe('login', function () {
    test('returns a token pair for valid credentials', function () {
        $user = User::factory()->create();

        $this->postJson('/api/v1/auth/login', ['email' => $user->email, 'password' => 'password'])
            ->assertOk()
            ->assertJsonStructure(['access_token', 'refresh_token', 'token_type', 'expires_in'])
            ->assertJson(['token_type' => 'Bearer', 'expires_in' => 900]);
    });

    test('returns 401 for invalid credentials', function (string $email, string $password) {
        User::factory()->create(['email' => 'ada@example.com']);

        $this->postJson('/api/v1/auth/login', ['email' => $email, 'password' => $password])
            ->assertUnauthorized()
            ->assertExactJson(['message' => 'Invalid credentials.']);
    })->with([
        'wrong password' => ['ada@example.com', 'wrong-password'],
        'unknown email' => ['nobody@example.com', 'password'],
    ]);

    test('returns 422 when the credentials are missing', function () {
        $this->postJson('/api/v1/auth/login')
            ->assertUnprocessable()
            ->assertJsonValidationErrors([
                'email' => 'The email field is required.',
                'password' => 'The password field is required.',
            ]);
    });

    test('returns 429 after too many attempts', function () {
        foreach (range(1, 10) as $attempt) {
            $this->postJson('/api/v1/auth/login', ['email' => 'nobody@example.com', 'password' => 'password'])
                ->assertUnauthorized();
        }

        $this->postJson('/api/v1/auth/login', ['email' => 'nobody@example.com', 'password' => 'password'])
            ->assertTooManyRequests();
    });
});

describe('me', function () {
    test('returns the authenticated user without their password', function () {
        $user = User::factory()->create();
        $tokens = tokensFor($user);

        $this->withToken($tokens['access_token'])->getJson('/api/v1/auth/me')
            ->assertOk()
            ->assertJson(['id' => $user->id, 'email' => $user->email])
            ->assertJsonMissingPath('password');
    });

    test('returns 401 when no token is provided', function () {
        $this->getJson('/api/v1/auth/me')->assertUnauthorized();
    });

    test('returns 401 when the token signature is invalid', function () {
        $tokens = tokensFor(User::factory()->create());

        $this->withToken($tokens['access_token'].'tampered')->getJson('/api/v1/auth/me')
            ->assertUnauthorized();
    });

    test('returns 401 when the access token has expired', function () {
        $tokens = tokensFor(User::factory()->create());

        $this->travel(16)->minutes();

        $this->withToken($tokens['access_token'])->getJson('/api/v1/auth/me')
            ->assertUnauthorized();
    });

    test('returns 401 when the user no longer exists', function () {
        $user = User::factory()->create();
        $tokens = tokensFor($user);
        $user->delete();

        $this->withToken($tokens['access_token'])->getJson('/api/v1/auth/me')
            ->assertUnauthorized();
    });
});

describe('refresh', function () {
    test('returns a new token pair', function () {
        $tokens = tokensFor(User::factory()->create());

        $response = $this->postJson('/api/v1/auth/refresh', ['refresh_token' => $tokens['refresh_token']]);

        $response->assertOk()
            ->assertJsonStructure(['access_token', 'refresh_token', 'token_type', 'expires_in']);
        expect($response->json('refresh_token'))->not->toBe($tokens['refresh_token']);
    });

    test('returns an access token for the same user', function () {
        $user = User::factory()->create();
        $tokens = tokensFor($user);
        $refreshed = $this->postJson('/api/v1/auth/refresh', ['refresh_token' => $tokens['refresh_token']])->json();

        $this->withToken($refreshed['access_token'])->getJson('/api/v1/auth/me')
            ->assertOk()
            ->assertJson(['id' => $user->id]);
    });

    test('returns 401 when the refresh token was already used', function () {
        $tokens = tokensFor(User::factory()->create());
        $this->postJson('/api/v1/auth/refresh', ['refresh_token' => $tokens['refresh_token']])->assertOk();

        $this->postJson('/api/v1/auth/refresh', ['refresh_token' => $tokens['refresh_token']])
            ->assertUnauthorized()
            ->assertExactJson(['message' => 'Invalid refresh token.']);
    });

    test('returns 401 for an unknown refresh token', function () {
        $this->postJson('/api/v1/auth/refresh', ['refresh_token' => 'not-a-real-token'])
            ->assertUnauthorized();
    });

    test('returns 401 when the refresh token has expired', function () {
        config(['jwt.refresh_ttl' => 15]);
        $tokens = tokensFor(User::factory()->create());

        $this->travel(16)->minutes();

        $this->postJson('/api/v1/auth/refresh', ['refresh_token' => $tokens['refresh_token']])
            ->assertUnauthorized();
    });

    test('returns 422 when the refresh token is missing', function () {
        $this->postJson('/api/v1/auth/refresh')
            ->assertUnprocessable()
            ->assertJsonValidationErrors(['refresh_token' => 'The refresh token field is required.']);
    });
});

describe('logout', function () {
    test('revokes the refresh token', function () {
        $tokens = tokensFor(User::factory()->create());

        $this->withToken($tokens['access_token'])
            ->postJson('/api/v1/auth/logout', ['refresh_token' => $tokens['refresh_token']])
            ->assertNoContent();

        $this->postJson('/api/v1/auth/refresh', ['refresh_token' => $tokens['refresh_token']])
            ->assertUnauthorized();
    });

    test('does not revoke a refresh token that belongs to another user', function () {
        $tokens = tokensFor(User::factory()->create());
        $otherTokens = tokensFor(User::factory()->create());

        $this->withToken($tokens['access_token'])
            ->postJson('/api/v1/auth/logout', ['refresh_token' => $otherTokens['refresh_token']])
            ->assertNoContent();

        $this->postJson('/api/v1/auth/refresh', ['refresh_token' => $otherTokens['refresh_token']])
            ->assertOk();
    });

    test('returns 401 when no access token is provided', function () {
        $this->postJson('/api/v1/auth/logout', ['refresh_token' => 'anything'])
            ->assertUnauthorized();
    });
});
