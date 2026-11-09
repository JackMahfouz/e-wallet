<?php

namespace Tests\Fakes;

use App\Services\TokenService;
use Illuminate\Support\Str;

/**
 * In-memory stand-in for the Go auth service. Token validity (signature, expiry) is that
 * service's job and is tested in libs/auth; here a token is valid if this fake issued it.
 */
class FakeTokenService implements TokenService
{
    public const EXPIRES_IN = 900;

    /** @var array<string, string> */
    private array $subjects = [];

    public function issue(string $subject): array
    {
        $token = 'fake-access-'.Str::random(32);
        $this->subjects[$token] = $subject;

        return ['access_token' => $token, 'expires_in' => self::EXPIRES_IN];
    }

    public function verify(string $token): ?string
    {
        return $this->subjects[$token] ?? null;
    }
}
