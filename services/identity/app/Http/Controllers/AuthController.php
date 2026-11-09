<?php

namespace App\Http\Controllers;

use App\Services\AuthService;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Http\Response;

class AuthController extends Controller
{
    public function __construct(private AuthService $authService) {}

    public function register(Request $request): JsonResponse
    {
        $attributes = $request->validate([
            'name' => ['required', 'string', 'max:255'],
            'email' => ['required', 'email', 'max:255', 'unique:users'],
            'password' => ['required', 'string', 'min:8'],
        ]);

        return response()->json($this->authService->register($attributes), 201);
    }

    public function login(Request $request): JsonResponse
    {
        $credentials = $request->validate([
            'email' => ['required', 'email'],
            'password' => ['required', 'string'],
        ]);

        $tokens = $this->authService->login($credentials['email'], $credentials['password']);

        if ($tokens === null) {
            return response()->json(['message' => 'Invalid credentials.'], 401);
        }

        return response()->json($tokens);
    }

    public function refresh(Request $request): JsonResponse
    {
        $validated = $request->validate(['refresh_token' => ['required', 'string']]);

        $tokens = $this->authService->refresh($validated['refresh_token']);

        if ($tokens === null) {
            return response()->json(['message' => 'Invalid refresh token.'], 401);
        }

        return response()->json($tokens);
    }

    public function logout(Request $request): Response
    {
        $validated = $request->validate(['refresh_token' => ['required', 'string']]);

        $this->authService->logout($request->user(), $validated['refresh_token']);

        return response()->noContent();
    }

    public function me(Request $request): JsonResponse
    {
        return response()->json($request->user());
    }
}
