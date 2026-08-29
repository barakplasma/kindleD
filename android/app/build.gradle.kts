plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.kindled.display"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.kindled.display"
        // API 30: GestureDescription.Builder.setDisplayId, which is what
        // makes injecting touches into the virtual display possible.
        minSdk = 30
        targetSdk = 35
        versionCode = 1
        versionName = "1.0"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    testOptions {
        unitTests.isReturnDefaultValues = true
    }
}

// No AndroidX, no Material, no third-party libraries: the UI is three
// buttons and a status line, and a dependency-free app is one less thing to
// go wrong in a hotel room with no internet.
dependencies {
    testImplementation("junit:junit:4.13.2")
}
