<?php

namespace App\Services;

interface TokenService
{
    /**
     * Ask the auth service for an access token for the given subject.
     *
     * @return array{access_token: string, expires_in: int}
     */
    public function issue(string $subject): array;

    /**
     * Return the subject of a valid, unexpired access token, or null when it is missing or not valid.
     */
    public function verify(string $token): ?string;
}
