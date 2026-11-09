<?php

use App\Models\User;
use App\Services\AuthService;
use App\Services\TokenService;
use Illuminate\Support\Facades\Exceptions;
use Illuminate\Support\Facades\Hash;
use Tests\Fakes\FakeTokenService;

beforeEach(function () {
    $this->app->instance(TokenService::class, new FakeTokenService);
});

/**
 * Sign in a factory user (their password is "password") without going through HTTP.
 *
 * @return array{access_token: string, refresh_token: string}
 */
function tokensFor(User $user): array
{
    return app(AuthService::class)->login($user->email, 'password');
}

/**
 * A token service whose backend is down: every call fails like an unreachable auth service.
 */
function unreachableTokenService(): TokenService
{
    return new class implements TokenService
    {
        public function issue(string $subject): array
        {
            throw new RuntimeException('auth service unreachable');
        }

        public function verify(string $token): ?string
        {
            throw new RuntimeException('auth service unreachable');
        }
    };
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

    // These two use unique emails: failures are recorded in the real Redis, so a fixed email
    // would pile up across test runs and eventually be locked out.
    test('returns 401 for a wrong password', function () {
        $user = User::factory()->create();

        $this->postJson('/api/v1/auth/login', ['email' => $user->email, 'password' => 'wrong-password'])
            ->assertUnauthorized()
            ->assertExactJson(['message' => 'Invalid credentials.']);
    });

    test('returns 401 for an unknown email', function () {
        $this->postJson('/api/v1/auth/login', ['email' => fake()->unique()->safeEmail(), 'password' => 'password'])
            ->assertUnauthorized()
            ->assertExactJson(['message' => 'Invalid credentials.']);
    });

    test('returns 500 and stores no refresh token when the auth service is unreachable', function () {
        Exceptions::fake();
        $user = User::factory()->create();
        $this->app->instance(TokenService::class, unreachableTokenService());

        $this->postJson('/api/v1/auth/login', ['email' => $user->email, 'password' => 'password'])
            ->assertInternalServerError();

        Exceptions::assertReported(fn (RuntimeException $e) => $e->getMessage() === 'auth service unreachable');
        $this->assertDatabaseCount('refresh_tokens', 0);
    });

    test('returns 422 when the credentials are missing', function () {
        $this->postJson('/api/v1/auth/login')
            ->assertUnprocessable()
            ->assertJsonValidationErrors([
                'email' => 'The email field is required.',
                'password' => 'The password field is required.',
            ]);
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

    // Signature and expiry checks belong to the auth service and are tested in libs/auth.
    test('returns 401 when the auth service does not accept the token', function () {
        $this->withToken('not-issued-by-anyone')->getJson('/api/v1/auth/me')
            ->assertUnauthorized();
    });

    test('returns 500 instead of letting the request through when the auth service is unreachable', function () {
        Exceptions::fake();
        $this->app->instance(TokenService::class, unreachableTokenService());

        $this->withToken('any-token')->getJson('/api/v1/auth/me')
            ->assertInternalServerError();

        Exceptions::assertReported(RuntimeException::class);
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
