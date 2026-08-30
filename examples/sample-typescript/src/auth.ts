export interface User { username: string }

export interface CredentialStore {
  /** Authenticate must use the application's password-hashing policy. */
  authenticate(username: string, password: string): User | undefined
}

export class AuthService {
  constructor(private readonly credentials: CredentialStore) {}

  login(username: string, password: string): User {
    if (!username.trim() || !password) throw new Error("invalid credentials")
    const user = this.credentials.authenticate(username, password)
    if (!user) throw new Error("invalid credentials")
    return user
  }
}

export function registerRoutes(app: any, service: AuthService): void {
  app.post("/login", (request: any, response: any) => {
    response.json(service.login(request.body.username, request.body.password))
  })
}
