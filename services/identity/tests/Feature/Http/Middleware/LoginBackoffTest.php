<?php

use App\Models\User;
use App\Services\TokenService;
use Illuminate\Testing\TestResponse;
use Tests\Fakes\FakeTokenService;

// These tests use the real local Redis. Every test uses its own emails, so state never leaks
// between tests or runs, and time is frozen so lockouts are exact.
beforeEach(function () {
    $this->app->instance(TokenService::class, new FakeTokenService);
    $this->freezeTime();
});

function loginWith(string $email, string $password): TestResponse
{
    return test()->postJson('/api/v1/auth/login', ['email' => $email, 'password' => $password]);
}

function failedLogin(string $email): TestResponse
{
    return loginWith($email, 'wrong-password');
}

test('returns 429 with Retry-After once the free attempts are used up', function () {
    config(['auth.login_backoff.free_attempts' => 2]);
    $email = fake()->unique()->safeEmail();

    failedLogin($email)->assertUnauthorized();
    failedLogin($email)->assertUnauthorized();
    failedLogin($email)->assertUnauthorized();

    failedLogin($email)
        ->assertTooManyRequests()
        ->assertHeader('Retry-After', '1')
        ->assertExactJson(['message' => 'Too many failed attempts. Try again later.']);
});

test('doubles the lockout after each further failed attempt', function () {
    config(['auth.login_backoff.free_attempts' => 0]);
    $email = fake()->unique()->safeEmail();

    foreach ([1, 2, 4, 8] as $seconds) {
        failedLogin($email)->assertUnauthorized();
        failedLogin($email)->assertTooManyRequests()->assertHeader('Retry-After', (string) $seconds);

        $this->travel($seconds)->seconds();
    }
});

test('caps the lockout at the configured maximum', function () {
    config(['auth.login_backoff.free_attempts' => 0, 'auth.login_backoff.max_seconds' => 3]);
    $email = fake()->unique()->safeEmail();

    foreach ([1, 2, 3, 3] as $seconds) {
        failedLogin($email)->assertUnauthorized();
        failedLogin($email)->assertTooManyRequests()->assertHeader('Retry-After', (string) $seconds);

        $this->travel($seconds)->seconds();
    }
});

test('rejects correct credentials while locked out, then accepts them after the delay', function () {
    config(['auth.login_backoff.free_attempts' => 0]);
    $user = User::factory()->create();
    failedLogin($user->email)->assertUnauthorized();

    loginWith($user->email, 'password')->assertTooManyRequests();

    $this->travel(1)->seconds();
    loginWith($user->email, 'password')->assertOk();
});

test('forgets earlier failures after a successful login', function () {
    config(['auth.login_backoff.free_attempts' => 1]);
    $user = User::factory()->create();
    failedLogin($user->email)->assertUnauthorized();
    loginWith($user->email, 'password')->assertOk();

    // Without the reset this would be the second failure and lock the account.
    failedLogin($user->email)->assertUnauthorized();

    loginWith($user->email, 'password')->assertOk();
});

test('locks out one email without affecting another', function () {
    config(['auth.login_backoff.free_attempts' => 0]);
    $locked = User::factory()->create();
    $other = User::factory()->create();
    failedLogin($locked->email)->assertUnauthorized();
    failedLogin($locked->email)->assertTooManyRequests();

    loginWith($other->email, 'password')->assertOk();
});

test('locks out per client address', function () {
    config(['auth.login_backoff.free_attempts' => 0]);
    $user = User::factory()->create();
    failedLogin($user->email)->assertUnauthorized();
    failedLogin($user->email)->assertTooManyRequests();

    $this->withServerVariables(['REMOTE_ADDR' => '203.0.113.9']);

    loginWith($user->email, 'password')->assertOk();
});

test('treats the email case-insensitively', function () {
    config(['auth.login_backoff.free_attempts' => 0]);
    $user = User::factory()->create();
    failedLogin(strtoupper($user->email))->assertUnauthorized();

    loginWith(strtolower($user->email), 'password')->assertTooManyRequests();
});

test('does not count validation errors as failed attempts', function () {
    config(['auth.login_backoff.free_attempts' => 0]);
    $user = User::factory()->create();
    $this->postJson('/api/v1/auth/login', ['email' => $user->email])->assertUnprocessable();

    loginWith($user->email, 'password')->assertOk();
});

test('returns 422 rather than failing when the email is not a string', function () {
    $this->postJson('/api/v1/auth/login', ['email' => ['not', 'a', 'string'], 'password' => 'password'])
        ->assertUnprocessable()
        ->assertJsonValidationErrors('email');
});
