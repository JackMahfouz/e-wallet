<?php

use App\Services\JwtService;

test('decodes the claims of a token it issued', function () {
    $claims = ['sub' => '1', 'exp' => now()->addMinute()->timestamp];
    $service = app(JwtService::class);

    $token = $service->encode($claims);

    expect($service->decode($token))->toBe($claims);
});

test('returns null for a malformed token', function (string $token) {
    expect(app(JwtService::class)->decode($token))->toBeNull();
})->with([
    'empty string' => '',
    'single part' => 'abc',
    'two parts' => 'abc.def',
    'four parts' => 'a.b.c.d',
    'empty parts' => '..',
]);

test('returns null when the payload was changed after signing', function () {
    $service = app(JwtService::class);
    $token = $service->encode(['sub' => '1', 'exp' => now()->addMinute()->timestamp]);
    [$header, , $signature] = explode('.', $token);
    $forgedPayload = rtrim(strtr(base64_encode(json_encode(['sub' => '2', 'exp' => now()->addMinute()->timestamp])), '+/', '-_'), '=');

    expect($service->decode("{$header}.{$forgedPayload}.{$signature}"))->toBeNull();
});

test('returns null when the token was signed with a different secret', function () {
    $service = app(JwtService::class);
    $token = $service->encode(['sub' => '1', 'exp' => now()->addMinute()->timestamp]);

    config(['jwt.secret' => 'a-different-secret']);

    expect($service->decode($token))->toBeNull();
});

test('returns null when the token has expired', function () {
    $service = app(JwtService::class);
    $token = $service->encode(['sub' => '1', 'exp' => now()->subMinute()->timestamp]);

    expect($service->decode($token))->toBeNull();
});

test('returns null when the token has no expiry', function () {
    $service = app(JwtService::class);
    $token = $service->encode(['sub' => '1']);

    expect($service->decode($token))->toBeNull();
});

test('throws instead of verifying when the secret is not configured', function (?string $secret) {
    config(['jwt.secret' => $secret]);

    expect(fn () => app(JwtService::class)->decode('a.b.c'))->toThrow(RuntimeException::class);
})->with([
    'missing' => null,
    'empty' => '',
]);
