<?php

namespace App\Services;

use RuntimeException;

class JwtService
{
    /**
     * Encode the claims into an HS256-signed JWT.
     *
     * @param  array<string, mixed>  $claims
     */
    public function encode(array $claims): string
    {
        $unsigned = $this->base64UrlEncode(json_encode(['typ' => 'JWT', 'alg' => 'HS256'], JSON_THROW_ON_ERROR))
            .'.'.$this->base64UrlEncode(json_encode($claims, JSON_THROW_ON_ERROR));

        return $unsigned.'.'.$this->base64UrlEncode($this->sign($unsigned));
    }

    /**
     * Return the claims of a token with a valid signature that has not expired, or null.
     *
     * @return array<string, mixed>|null
     */
    public function decode(string $token): ?array
    {
        $parts = explode('.', $token);

        if (count($parts) !== 3) {
            return null;
        }

        [$header, $payload, $signature] = $parts;

        if (! hash_equals($this->base64UrlEncode($this->sign("{$header}.{$payload}")), $signature)) {
            return null;
        }

        $json = base64_decode(strtr($payload, '-_', '+/'), true);
        $claims = $json === false ? null : json_decode($json, true);

        if (! is_array($claims) || ! is_int($claims['exp'] ?? null) || $claims['exp'] <= now()->timestamp) {
            return null;
        }

        return $claims;
    }

    private function sign(string $data): string
    {
        $secret = config('jwt.secret');

        if (! is_string($secret) || $secret === '') {
            throw new RuntimeException('The JWT secret is not configured. Set JWT_SECRET.');
        }

        return hash_hmac('sha256', $data, $secret, true);
    }

    private function base64UrlEncode(string $data): string
    {
        return rtrim(strtr(base64_encode($data), '+/', '-_'), '=');
    }
}
