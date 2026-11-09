<?php

namespace App\Services;

use App\Models\RefreshToken;
use App\Models\User;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Hash;
use Illuminate\Support\Str;

/**
 * @phpstan-type TokenPair array{access_token: string, refresh_token: string, token_type: string, expires_in: int}
 */
class AuthService
{
    public function __construct(private TokenService $tokenService) {}

    /**
     * Create a user and sign them in.
     *
     * @param  array{name: string, email: string, password: string}  $attributes
     * @return TokenPair
     */
    public function register(array $attributes): array
    {
        return $this->issueTokens(User::create($attributes));
    }

    /**
     * Exchange credentials for a token pair, or null when they are wrong.
     *
     * @return TokenPair|null
     */
    public function login(string $email, string $password): ?array
    {
        $user = User::where('email', $email)->first();

        if ($user === null || ! Hash::check($password, $user->password)) {
            return null;
        }

        return $this->issueTokens($user);
    }

    /**
     * Revoke a live refresh token and issue a new pair, or null when it is unknown, used, or expired.
     *
     * @return TokenPair|null
     */
    public function refresh(string $refreshToken): ?array
    {
        return DB::transaction(function () use ($refreshToken): ?array {
            $token = RefreshToken::where('token_hash', $this->hashToken($refreshToken))
                ->whereNull('revoked_at')
                ->where('expires_at', '>', now())
                ->lockForUpdate()
                ->first();

            if ($token === null) {
                return null;
            }

            $token->update(['revoked_at' => now()]);

            return $this->issueTokens($token->user);
        });
    }

    /**
     * Revoke one of the user's refresh tokens.
     */
    public function logout(User $user, string $refreshToken): void
    {
        $user->refreshTokens()
            ->where('token_hash', $this->hashToken($refreshToken))
            ->whereNull('revoked_at')
            ->update(['revoked_at' => now()]);
    }

    /**
     * @return TokenPair
     */
    private function issueTokens(User $user): array
    {
        // Ask the auth service first, so a failed call leaves no orphaned refresh token behind.
        $accessToken = $this->tokenService->issue((string) $user->id);
        $refreshToken = Str::random(64);

        $user->refreshTokens()->create([
            'token_hash' => $this->hashToken($refreshToken),
            'expires_at' => now()->addMinutes(config('jwt.refresh_ttl')),
        ]);

        return [
            'access_token' => $accessToken['access_token'],
            'refresh_token' => $refreshToken,
            'token_type' => 'Bearer',
            'expires_in' => $accessToken['expires_in'],
        ];
    }

    private function hashToken(string $token): string
    {
        return hash('sha256', $token);
    }
}
